/**
 * React 19 的 @types/react 不再向全局注入 JSX 命名空间，
 * 而本项目的组件沿用了 `JSX.Element` 这种写法，这里把它补回来，
 * 避免每个文件都要 import type { JSX } from 'react'。
 */
import type { JSX as ReactJSX } from 'react'
import type { DesktopApi } from './api'

declare global {
  interface Window {
    /** Wails 桥接层（src/api）挂载的宿主 API，与原 Electron preload 同构 */
    api: DesktopApi
  }

  namespace JSX {
    type Element = ReactJSX.Element
    type ElementType = ReactJSX.ElementType
    interface ElementClass extends ReactJSX.ElementClass {}
    interface ElementAttributesProperty extends ReactJSX.ElementAttributesProperty {}
    interface ElementChildrenAttribute extends ReactJSX.ElementChildrenAttribute {}
    interface IntrinsicAttributes extends ReactJSX.IntrinsicAttributes {}
    interface IntrinsicClassAttributes<T> extends ReactJSX.IntrinsicClassAttributes<T> {}
    interface IntrinsicElements extends ReactJSX.IntrinsicElements {}
  }
}

export {}
