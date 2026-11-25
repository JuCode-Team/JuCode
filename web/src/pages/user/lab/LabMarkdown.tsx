import { isValidElement, useEffect, useRef, useState, type ReactNode } from "react";
import Markdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { Check, Code, Copy, Eye } from "lucide-react";
import { useTranslation } from "react-i18next";
import { copyText } from "../../../lib/copy";

// Markdown for lab answers and fetched pages: GFM, highlighted code with a
// copy button (an HTML block can also be previewed), links in a new tab.
// Token colours live in lab.css.

const remarkPlugins = [remarkGfm];
const noPlugins: [] = [];

// The highlighter (highlight.js common languages, ~50 KB gzip) loads only
// once some text actually contains a fenced code block.
type Highlighter = typeof import("rehype-highlight").default;
let highlighter: Highlighter | null = null;
let loadingHighlighter: Promise<void> | null = null;

function useHighlighter(needed: boolean): Highlighter | null {
  const [, setLoaded] = useState(false);
  useEffect(() => {
    if (!needed || highlighter) return;
    let live = true;
    loadingHighlighter ??= import("rehype-highlight").then((mod) => { highlighter = mod.default; });
    loadingHighlighter.then(() => { if (live) setLoaded(true); }).catch(() => { /* plain code blocks still render */ });
    return () => { live = false; };
  }, [needed]);
  return needed ? highlighter : null;
}

function codeLanguage(children: ReactNode): string {
  if (!isValidElement<{ className?: string }>(children)) return "";
  const match = /language-([\w+-]+)/.exec(children.props.className ?? "");
  return match ? match[1] : "";
}

const PREVIEWABLE = new Set(["html", "htm", "svg"]);

function CodeBlock({ children }: { children?: ReactNode }) {
  const { t } = useTranslation();
  const preRef = useRef<HTMLPreElement>(null);
  const [copied, setCopied] = useState(false);
  // The page shown in the preview: the block's text when it was opened.
  const [preview, setPreview] = useState<string | null>(null);
  const lang = codeLanguage(children);
  const previewable = PREVIEWABLE.has(lang.toLowerCase());
  return (
    <div className="jc-md-code">
      <div className="jc-md-code-bar">
        <span>{lang || t("bench.markdown.code")}</span>
        {previewable && (
          <button type="button" aria-pressed={preview !== null} onClick={() => setPreview(preview === null ? (preRef.current?.textContent ?? "") : null)}>
            {preview !== null ? <Code size={12} /> : <Eye size={12} />}
            {preview !== null ? t("bench.markdown.code") : t("bench.markdown.preview")}
          </button>
        )}
        <button
          type="button"
          aria-label={t("bench.markdown.copyCode")}
          onClick={async () => {
            // While previewing, the code is not in the page: copy it from the preview's source.
            if (await copyText(preview ?? preRef.current?.textContent ?? "")) {
              setCopied(true);
              window.setTimeout(() => setCopied(false), 1400);
            }
          }}
        >
          {copied ? <Check size={12} /> : <Copy size={12} />}
          {copied ? t("bench.markdown.copied") : t("bench.markdown.copy")}
        </button>
      </div>
      {/* Scripts run, but in an opaque origin: no access to this page,
          its storage or the session token. */}
      {preview !== null ? (
        <iframe className="jc-md-preview" title={t("bench.markdown.htmlPreview")} sandbox="allow-scripts" srcDoc={preview} />
      ) : (
        <pre ref={preRef}>{children}</pre>
      )}
    </div>
  );
}

const baseComponents: Components = {
  pre: ({ children }) => <CodeBlock>{children}</CodeBlock>,
  a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>,
};

export function LabMarkdown({ children, img }: { children: string; img?: Components["img"] }) {
  const highlight = useHighlighter(/(^|\n) {0,3}(```|~~~)/.test(children));
  return (
    <Markdown
      remarkPlugins={remarkPlugins}
      rehypePlugins={highlight ? [highlight] : noPlugins}
      components={img ? { ...baseComponents, img } : baseComponents}
    >
      {children}
    </Markdown>
  );
}
