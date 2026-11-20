// crypto.go：猫协议加解密族全局函数（md5/sha/base64/hex/aes/des/rc4）。
// 全部用标准库实现，AES/DES 分组模式手写 CBC/ECB + PKCS7（不引第三方）。
//
// 作者：Shiro
package bridge

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// MD5 十六进制摘要（猫协议 md5 全局函数）。
func MD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SHA1 十六进制摘要。
func SHA1(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SHA256 十六进制摘要。
func SHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Base64Encode UTF-8 字节标准 base64。
func Base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// Base64Decode 非法输入原样返回（猫脚本常对已解码内容二次调用）。
func Base64Decode(s string) string {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return string(b)
}

// HexEncode UTF-8 字节的十六进制串。
func HexEncode(s string) string {
	return hex.EncodeToString([]byte(s))
}

// HexDecode 非法输入原样返回。
func HexDecode(s string) string {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return string(b)
}

// keyBytes 密钥解析：优先 UTF-8 原始字节；长度不符再按 hex 解码（猫源两种写法都有）。
func keyBytes(key string, sizes ...int) ([]byte, error) {
	raw := []byte(key)
	for _, n := range sizes {
		if len(raw) == n {
			return raw, nil
		}
	}
	if h, err := hex.DecodeString(strings.TrimSpace(key)); err == nil {
		for _, n := range sizes {
			if len(h) == n {
				return h, nil
			}
		}
	}
	return nil, fmt.Errorf("密钥长度非法（%d 字节），期望 %v 之一", len(raw), sizes)
}

// pkcs7Pad PKCS7 填充。
func pkcs7Pad(data []byte, blockSize int) []byte {
	n := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(n)}, n)...)
}

// pkcs7Unpad 去 PKCS7 填充，校验失败报错。
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("密文为空")
	}
	n := int(data[len(data)-1])
	if n <= 0 || n > len(data) {
		return nil, fmt.Errorf("PKCS7 填充非法")
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, fmt.Errorf("PKCS7 填充非法")
		}
	}
	return data[:len(data)-n], nil
}

// blockCrypt 对 PKCS7 填充后的数据做 CBC/ECB 单向加解密（分块循环，不引第三方模式库）。
func blockCrypt(block cipher.Block, mode string, in, iv []byte, encrypt bool) ([]byte, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = "cbc"
	}
	out := make([]byte, len(in))
	switch m {
	case "cbc":
		if len(iv) != block.BlockSize() {
			return nil, fmt.Errorf("CBC 模式 IV 需 %d 字节，实际 %d", block.BlockSize(), len(iv))
		}
		if encrypt {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, in)
		} else {
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, in)
		}
	case "ecb":
		for i := 0; i+block.BlockSize() <= len(in); i += block.BlockSize() {
			if encrypt {
				block.Encrypt(out[i:i+block.BlockSize()], in[i:i+block.BlockSize()])
			} else {
				block.Decrypt(out[i:i+block.BlockSize()], in[i:i+block.BlockSize()])
			}
		}
	default:
		return nil, fmt.Errorf("不支持的模式：%s", mode)
	}
	return out, nil
}

// AesEncode 明文 AES 加密（mode：cbc 默认 / ecb，PKCS7），输出 base64。
func AesEncode(plain, key, iv, mode string) (string, error) {
	keyB, err := keyBytes(key, 16, 24, 32)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(keyB)
	if err != nil {
		return "", err
	}
	ivB, _ := keyBytes(iv, 16) // ECB 不校验 IV 长度
	data := pkcs7Pad([]byte(plain), block.BlockSize())
	out, err := blockCrypt(block, mode, data, ivB, true)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// AesDecode base64 密文 AES 解密，返回明文（失败报错，由脚本自行兜底）。
func AesDecode(b64Cipher, key, iv, mode string) (string, error) {
	keyB, err := keyBytes(key, 16, 24, 32)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(keyB)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Cipher))
	if err != nil {
		return "", fmt.Errorf("密文非合法 base64：%w", err)
	}
	if len(raw)%block.BlockSize() != 0 {
		return "", fmt.Errorf("密文长度 %d 不是分组大小 %d 的整数倍", len(raw), block.BlockSize())
	}
	ivB, _ := keyBytes(iv, 16)
	out, err := blockCrypt(block, mode, raw, ivB, false)
	if err != nil {
		return "", err
	}
	plain, err := pkcs7Unpad(out)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// DesEncode 明文 3DES 加密（key 24 字节或 DES 8 字节自动区分），输出 base64。
func DesEncode(plain, key, iv, mode string) (string, error) {
	block, err := newDesBlock(key)
	if err != nil {
		return "", err
	}
	ivB, _ := keyBytes(iv, 8)
	data := pkcs7Pad([]byte(plain), block.BlockSize())
	out, err := blockCrypt(block, mode, data, ivB, true)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// DesDecode base64 密文 DES/3DES 解密。
func DesDecode(b64Cipher, key, iv, mode string) (string, error) {
	block, err := newDesBlock(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Cipher))
	if err != nil {
		return "", fmt.Errorf("密文非合法 base64：%w", err)
	}
	if len(raw)%block.BlockSize() != 0 {
		return "", fmt.Errorf("密文长度 %d 不是分组大小 %d 的整数倍", len(raw), block.BlockSize())
	}
	ivB, _ := keyBytes(iv, 8)
	out, err := blockCrypt(block, mode, raw, ivB, false)
	if err != nil {
		return "", err
	}
	plain, err := pkcs7Unpad(out)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// newDesBlock 按密钥长度选 DES（8 字节）或 3DES（24 字节）。
func newDesBlock(key string) (cipher.Block, error) {
	if k, err := keyBytes(key, 24); err == nil {
		return des.NewTripleDESCipher(k)
	}
	k, err := keyBytes(key, 8)
	if err != nil {
		return nil, err
	}
	return des.NewCipher(k)
}

// Rc4 RC4 流加密，输出 base64（脚本常再自行转 hex）。
func Rc4(text, key string) (string, error) {
	c, err := rc4.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	out := make([]byte, len(text))
	c.XORKeyStream(out, []byte(text))
	return base64.StdEncoding.EncodeToString(out), nil
}
