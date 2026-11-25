package llm

import "strings"

// Codex / OpenAI Responses built-in tools besides web_search.
const (
	builtinFileSearch       = "file_search"
	builtinFileSearchCall   = "file_search_call"
	builtinCodeExecution    = "code_execution"
	builtinCodeInterpreter  = "code_interpreter"
	builtinCodeExecutionAnn = "code_execution_20250522"
	builtinComputerUse      = "computer_use"
	builtinComputer         = "computer"
	builtinComputerUsePrev  = "computer_use_preview"
	builtinComputerAnn      = "computer_20250124"
	builtinImageGeneration  = "image_generation"
	builtinImageGen         = "image_gen"
	builtinLocalShell       = "local_shell"
	builtinGoogleSearch     = "google_search"
)

func isBuiltInToolType(t string) bool {
	if isWebSearchToolType(t) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(t)) {
	case builtinFileSearch, builtinFileSearchCall,
		builtinCodeExecution, builtinCodeInterpreter, builtinCodeExecutionAnn,
		builtinComputerUse, builtinComputer, builtinComputerUsePrev, builtinComputerAnn,
		builtinImageGeneration, builtinImageGen,
		builtinLocalShell, builtinGoogleSearch, "google_search_retrieval":
		return true
	default:
		return false
	}
}

func isBuiltInTool(t map[string]any) bool {
	if t == nil {
		return false
	}
	if isWebSearchTool(t) {
		return true
	}
	if isBuiltInToolType(strVal(t["type"])) {
		return true
	}
	name := strVal(t["name"])
	if fn := asMap(t["function"]); fn != nil && name == "" {
		name = strVal(fn["name"])
	}
	return isBuiltInToolType(name)
}

func passthroughBuiltInTool(t map[string]any) map[string]any {
	if t == nil {
		return nil
	}
	if isWebSearchTool(t) {
		return nil // callers handle web_search separately
	}
	out := asMap(cloneAny(t))
	if out == nil {
		return map[string]any{"type": strVal(t["type"])}
	}
	return out
}

func extractBuiltInToolCounts(obj map[string]any) map[string]int {
	out := map[string]int{}
	if obj == nil {
		return out
	}
	collect := func(usage map[string]any) {
		if usage == nil {
			return
		}
		if n := intVal(usage["file_search_requests"]); n > 0 {
			out[builtinFileSearch] = maxInt(out[builtinFileSearch], n)
		}
		if n := intVal(usage["code_execution_requests"]); n > 0 {
			out[builtinCodeExecution] = maxInt(out[builtinCodeExecution], n)
		}
		if n := intVal(usage["computer_use_requests"]); n > 0 {
			out[builtinComputerUse] = maxInt(out[builtinComputerUse], n)
		}
		if stu := asMap(usage["server_tool_use"]); stu != nil {
			if n := intVal(stu["web_search_requests"]); n > 0 {
				out[openaiWebSearch] = maxInt(out[openaiWebSearch], n)
			}
			if n := intVal(stu["file_search_requests"]); n > 0 {
				out[builtinFileSearch] = maxInt(out[builtinFileSearch], n)
			}
			if n := intVal(stu["code_execution_requests"]); n > 0 {
				out[builtinCodeExecution] = maxInt(out[builtinCodeExecution], n)
			}
		}
	}
	collect(asMap(obj["usage"]))
	if msg := asMap(obj["message"]); msg != nil {
		collect(asMap(msg["usage"]))
	}
	if resp := asMap(obj["response"]); resp != nil {
		collect(asMap(resp["usage"]))
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
