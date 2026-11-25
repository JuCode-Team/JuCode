// Minimal pub/sub so non-React modules (the query client's mutation cache)
// can raise the toast that GlobalErrorToast renders. A store would work too,
// but the toast has no readers other than that one component.

export interface AppToast {
  title: string;
  message: string;
}

type Listener = (toast: AppToast) => void;

const listeners = new Set<Listener>();

export function showToast(toast: AppToast) {
  listeners.forEach((listener) => listener(toast));
}

export function subscribeToast(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
