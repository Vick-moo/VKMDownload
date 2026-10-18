import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";

interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

/**
 * Last-resort catch for render errors.
 *
 * Without a boundary an exception anywhere in the tree unmounts the whole
 * React root, leaving an empty white window — and the desktop shell has no
 * address bar to reload from, so the only way back is killing the process. The
 * fallback keeps a usable frame on screen and offers a reload; the error itself
 * still reaches the console for a bug report.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error("VKMDownload 界面出现未捕获的错误：", error, info);
  }

  render(): ReactNode {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <div className="fatal" role="alert">
        <h1 className="fatal__title">界面出错了</h1>
        <p className="fatal__hint">
          下载任务和设置不会因此丢失。重新加载界面即可继续使用；如果反复出现，
          请把下面的信息反馈给我们。
        </p>
        <pre className="fatal__detail">{error.message || String(error)}</pre>
        <button
          type="button"
          className="btn btn--primary"
          onClick={() => window.location.reload()}
        >
          重新加载
        </button>
      </div>
    );
  }
}
