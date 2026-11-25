// 兄弟站点地址（文档站等）——按当前访问的域名推导，不写死。
//
// 同一套前端可以同时挂在多个注册域上（VITE_SITE_DOMAINS），每个注册域各有一套
// 子域。站内外链如果写死某一个域，用户就会被悄悄搬到另一个域上。
//
// 只认已配置的注册域，未命中（本地开发、预览环境）回落到 VITE_SITE_DEFAULT_DOMAIN。
import { deployment } from "../config/env";

/** 当前访问域名所属的注册域；未命中返回默认注册域（未配置时为空）。 */
export function currentRoot(hostname = typeof location === "undefined" ? "" : location.hostname): string {
  const h = hostname.toLowerCase();
  return deployment.siteDomains.find((r) => h === r || h.endsWith(`.${r}`)) ?? deployment.defaultDomain;
}

/** 同一注册域下的兄弟子域主机名，例如 siblingHost("docs") → "docs.example.com"。 */
export function siblingHost(sub: string, hostname?: string): string {
  return `${sub}.${currentRoot(hostname)}`;
}

/** 同一注册域下的兄弟子域地址，例如 siblingUrl("docs") → "https://docs.example.com"。 */
export function siblingUrl(sub: string, hostname?: string): string {
  return `https://${siblingHost(sub, hostname)}`;
}

/** 文档站地址：配置了 VITE_DOCS_URL 就用它，否则是当前注册域的 docs 子域；都没有时为空，调用方据此隐藏入口。 */
export function docsUrl(hostname?: string): string {
  if (deployment.docsUrl) return deployment.docsUrl;
  return currentRoot(hostname) ? siblingUrl("docs", hostname) : "";
}
