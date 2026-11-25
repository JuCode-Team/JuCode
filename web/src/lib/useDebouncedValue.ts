import { useEffect, useState } from "react";

// useDebouncedValue trails `value` by `ms`. Filter text inputs feed the
// raw value to the control and the debounced value to the query key, so a
// request goes out only after the operator stops typing.
export function useDebouncedValue<T>(value: T, ms = 300): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = window.setTimeout(() => setDebounced(value), ms);
    return () => window.clearTimeout(t);
  }, [value, ms]);
  return debounced;
}
