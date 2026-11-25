// usePagedQuery wraps useQuery for server-paginated lists. The backend
// is expected to take (q, limit, offset) query params and return a
// shape like { items: T[], total: number, limit: number, offset: number }
// (any item field name is fine — caller supplies the selector).
//
// Why a hook instead of a component prop: we want the DataTable to
// stay presentational. The hook owns state (q + page) and exposes
// debounced search, page navigation, and the React Query result.

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";

export interface PagedFetchResult<T> {
  items: T[];
  total: number;
  limit: number;
  offset: number;
}

export interface UsePagedQueryOptions<T> {
  // queryKey base; the hook appends [q, page, limit, ...extras]
  queryKey: readonly unknown[];
  // The fetcher receives the current params. Throw / reject on error.
  queryFn: (params: { q: string; limit: number; offset: number }) => Promise<PagedFetchResult<T>>;
  // Extra dependencies that should invalidate the query (e.g. filter
  // dropdown selections). Their values land in the queryKey verbatim.
  extras?: readonly unknown[];
  // Search input debounce ms. Default 250.
  debounceMs?: number;
  // Default page size. Default 50.
  pageSize?: number;
  // Refetch interval (passed straight to useQuery). 0/undefined disables.
  refetchInterval?: number | false;
  // Enable/disable like useQuery.enabled.
  enabled?: boolean;
}

export function usePagedQuery<T>(opts: UsePagedQueryOptions<T>) {
  const pageSize = opts.pageSize ?? 50;
  const debounceMs = opts.debounceMs ?? 250;
  const [rawQ, setRawQ] = useState("");
  const [q, setQ] = useState(""); // debounced
  const [page, setPage] = useState(0);
  const extras = useMemo(() => opts.extras ?? [], [opts.extras]);
  const extrasKey = useMemo(() => JSON.stringify(extras), [extras]);

  // Debounce search input → only refetch after the user stops typing.
  useEffect(() => {
    const t = window.setTimeout(() => setQ(rawQ.trim()), debounceMs);
    return () => window.clearTimeout(t);
  }, [rawQ, debounceMs]);

  // Any change in q OR extras must reset to first page; otherwise the
  // user lands on "page 5 of 0 results" which is confusing.
  useEffect(() => { setPage(0); }, [q, extrasKey]);

  const limit = pageSize;
  const offset = page * pageSize;

  const queryKey = useMemo(
    () => [...opts.queryKey, q, limit, offset, ...extras],
    [opts.queryKey, q, limit, offset, extras],
  );

  const query = useQuery<PagedFetchResult<T>>({
    queryKey,
    queryFn: () => opts.queryFn({ q, limit, offset }),
    placeholderData: (prev) => prev,
    refetchInterval: opts.refetchInterval ?? false,
    enabled: opts.enabled ?? true,
  });

  const total = query.data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  return {
    rawQ, setRawQ,
    q, // debounced value, mostly for display
    page, setPage,
    pageSize,
    total,
    totalPages,
    hasPrev: page > 0,
    hasNext: offset + pageSize < total,
    items: query.data?.items ?? [],
    query,
    // The fully-resolved key (base + debounced q + paging + extras). Exposed so
    // callers can write optimistic updates against the exact page in cache;
    // invalidating the base key alone would refetch and flash the old order.
    queryKey,
  };
}
