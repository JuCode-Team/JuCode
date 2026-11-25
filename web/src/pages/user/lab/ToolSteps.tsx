import { useEffect, useState } from "react";
import { ChevronDown, ChevronRight, FileText, Globe, Image as ImageIcon, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { StoredToolCall } from "../../../lib/labCache";

// Tool activity inside an assistant message: one compact line per call.
// Search lines open to list their hits.

const ICON = { web_search: Globe, web_fetch: FileText, generate_image: ImageIcon } as const;

function StepLine({ tool }: { tool: StoredToolCall }) {
  const { t } = useTranslation();
  const Icon = ICON[tool.name] ?? Globe;
  return (
    <>
      {tool.status === "running" ? <Loader2 size={13} className="jc-lab-step-spin" /> : <Icon size={13} />}
      <span className="jc-chatbox-toolcall-name">{t(`bench.steps.${tool.name}`, { defaultValue: tool.name })}{t("bench.steps.sep")}</span>
      <span className="jc-chatbox-toolcall-detail" title={tool.detail}>{tool.detail || "…"}</span>
      {tool.status === "done" && tool.summary && <span className="jc-lab-step-summary" title={tool.summary}>· {tool.summary}</span>}
      {tool.status === "error" && <span className="jc-lab-step-error" title={tool.error}>· {tool.error || t("bench.steps.failed")}</span>}
    </>
  );
}

function Step({ tool }: { tool: StoredToolCall }) {
  const hits = tool.results ?? [];
  if (hits.length === 0) {
    return <div className="jc-chatbox-toolcall"><span className="jc-lab-step-caret" /><StepLine tool={tool} /></div>;
  }
  return (
    <details className="jc-lab-step">
      <summary className="jc-chatbox-toolcall">
        <ChevronRight size={12} className="jc-lab-step-caret" />
        <StepLine tool={tool} />
      </summary>
      <ol className="jc-lab-step-hits">
        {hits.map((hit, i) => (
          <li key={`${hit.url}-${i}`}>
            <a href={hit.url} target="_blank" rel="noopener noreferrer" title={hit.url}>{hit.title}</a>
          </li>
        ))}
      </ol>
    </details>
  );
}

export function ToolSteps({ tools, streaming }: { tools: StoredToolCall[]; streaming: boolean }) {
  const { t } = useTranslation();
  const many = tools.length > 2;
  const [open, setOpen] = useState(() => streaming || !many);
  // Long runs fold away once the answer is done; the user can reopen them.
  useEffect(() => { if (!streaming && many) setOpen(false); }, [streaming, many]);
  if (tools.length === 0) return null;
  if (!many) {
    return <div className="jc-chatbox-toolcalls">{tools.map((tool) => <Step key={tool.id} tool={tool} />)}</div>;
  }
  const running = tools.some((tool) => tool.status === "running");
  const failed = tools.filter((tool) => tool.status === "error").length;
  return (
    <div className="jc-chatbox-toolcalls">
      <button type="button" className="jc-lab-steps-toggle" aria-expanded={open} onClick={() => setOpen(!open)}>
        {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
        {t("bench.steps.summary", { count: tools.length })}{running ? t("bench.steps.running") : ""}{failed ? t("bench.steps.failedCount", { count: failed }) : ""}
      </button>
      {open && <div className="jc-lab-steps">{tools.map((tool) => <Step key={tool.id} tool={tool} />)}</div>}
    </div>
  );
}
