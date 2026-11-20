/**
 * TVBox 猫协议宿主兼容层（QuickJS）。
 * 把脚本依赖的全局函数（req/pdfh/加解密族/log 等）转发到 Go 侧 __go_* 桥；
 * 网络、解析、加解密全部在宿主进程完成，脚本本身不接触任何系统能力。
 *
 * @author Shiro
 */
(function () {
	'use strict';

	function str(x) {
		return x === undefined || x === null ? '' : String(x);
	}

	function parseJSON(s) {
		try {
			return JSON.parse(s);
		} catch (e) {
			throw new Error('宿主函数返回异常: ' + str(s));
		}
	}

	// req 专用：成功返回 {code,headers,body}，失败返回 {ok:false,error}
	function unwrapReq(s) {
		const r = parseJSON(s);
		if (r && r.ok === false) {
			throw new Error(r.error || 'req 执行失败');
		}
		return r;
	}

	// 信封型宿主函数：{ok:true,v} / {ok:false,error}
	function unwrapV(s) {
		const r = parseJSON(s);
		if (r && r.ok === false) {
			throw new Error(r.error || '宿主函数执行失败');
		}
		return r.v;
	}

	globalThis.req = function (url, options) {
		return unwrapReq(__go_req(str(url), JSON.stringify(options || {})));
	};

	globalThis.pdfh = function (html, rule) {
		return str(__go_pdfh(str(html), str(rule)));
	};
	globalThis.pd = function (html, rule) {
		return str(__go_pd(str(html), str(rule)));
	};

	globalThis.md5 = (s) => str(__go_md5(str(s)));
	globalThis.sha1 = (s) => str(__go_sha1(str(s)));
	globalThis.sha256 = (s) => str(__go_sha256(str(s)));
	globalThis.base64Encode = (s) => str(__go_b64enc(str(s)));
	globalThis.base64Decode = (s) => str(__go_b64dec(str(s)));
	globalThis.hexEncode = (s) => str(__go_hexenc(str(s)));
	globalThis.hexDecode = (s) => str(__go_hexdec(str(s)));

	globalThis.aesEncode = (text, key, iv, mode) =>
		str(unwrapV(__go_aes_enc(str(text), str(key), str(iv), str(mode || 'cbc'))));
	globalThis.aesDecode = (b64, key, iv, mode) =>
		str(unwrapV(__go_aes_dec(str(b64), str(key), str(iv), str(mode || 'cbc'))));
	globalThis.desEncode = (text, key, iv, mode) =>
		str(unwrapV(__go_des_enc(str(text), str(key), str(iv), str(mode || 'cbc'))));
	globalThis.desDecode = (b64, key, iv, mode) =>
		str(unwrapV(__go_des_dec(str(b64), str(key), str(iv), str(mode || 'cbc'))));
	globalThis.rc4 = (text, key) => str(unwrapV(__go_rc4(str(text), str(key))));

	globalThis.gzipEncode = (s) => str(__go_gzip_enc(str(s)));
	globalThis.gzipDecode = (s) => str(__go_gzip_dec(str(s)));
	globalThis.zlibEncode = (s) => str(__go_zlib_enc(str(s)));
	globalThis.zlibDecode = (s) => str(__go_zlib_dec(str(s)));

	globalThis.urlEncode = (s) => encodeURIComponent(str(s));
	globalThis.urlDecode = (s) => {
		try {
			return decodeURIComponent(str(s));
		} catch (e) {
			return str(s);
		}
	};

	// 媒体地址统一走同源 /media 中间件（CORS/防盗链天然解决）
	globalThis.getProxyUrl = (u) => '/media?u=' + encodeURIComponent(str(u));

	function fmtLog(x) {
		if (typeof x === 'object') {
			try {
				return JSON.stringify(x);
			} catch (e) {
				return String(x);
			}
		}
		return String(x);
	}
	globalThis.log = function () {
		const parts = [];
		for (let i = 0; i < arguments.length; i++) {
			parts.push(fmtLog(arguments[i]));
		}
		__go_log(parts.join(' '));
	};
	globalThis.console = {
		log: globalThis.log,
		error: globalThis.log,
		warn: globalThis.log,
		info: globalThis.log,
		debug: globalThis.log
	};

	// QuickJS 无事件循环：setTimeout 简化为当前 job 之后立即执行。
	// 猫脚本绝大多数用它包 Promise 延迟，语义近似；真需要定时的脚本 P6 样本回归再补。
	globalThis.setTimeout = function (fn) {
		Promise.resolve().then(function () {
			try {
				fn();
			} catch (e) {
				globalThis.log('setTimeout error: ' + e);
			}
		});
		return 0;
	};
	globalThis.clearTimeout = function () {};

	// 统一调用面：main(input) 协议优先，退老式 init() 全局函数
	globalThis.__spider_wrap = async function (input) {
		if (typeof main === 'function') {
			const r = await main(input);
			return typeof r === 'string' ? r : JSON.stringify(r === undefined ? {} : r);
		}
		let obj = {};
		try {
			obj = JSON.parse(input);
		} catch (e) {}
		if (obj.type === 'init' && typeof init === 'function') {
			init(obj.ext, obj.group);
			return '{}';
		}
		throw new Error('脚本缺少 main(input) 入口');
	};
})();
