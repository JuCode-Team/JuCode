// Bridge between the axios interceptor and the step-up dialog.
//
// Sensitive admin actions (topup, settle, mark-paid, role change, password
// reset, redeem-code minting) answer 401 `totp_stepup_required` when the
// caller has TOTP bound and no fresh verification on file. Rather than
// teaching every call site to pass X-TOTP-Code, the interceptor asks for a
// code here and replays the request — one place, all endpoints.
//
// The UI half registers a prompt at mount; with none registered the request
// simply fails as before, so a code path that runs outside the React tree
// degrades instead of hanging.

export type StepUpPrompt = (opts: { retry: boolean }) => Promise<string | null>;

let prompt: StepUpPrompt | null = null;

export function registerStepUpPrompt(fn: StepUpPrompt | null): void {
  prompt = fn;
}

export function stepUpPromptAvailable(): boolean {
  return prompt !== null;
}

// Prompts are serialized: two sensitive requests failing at once must not
// stack two dialogs on top of each other. The second one waits, and by the
// time it runs the first verification has usually opened the server-side
// step-up window, so it often succeeds on the code the user types next.
let queue: Promise<unknown> = Promise.resolve();

// requestStepUpCode resolves with the code the user typed, or null when they
// dismissed the dialog (or no dialog is mounted). `retry` is true when the
// previous code was rejected, so the dialog can say so.
export function requestStepUpCode(retry: boolean): Promise<string | null> {
  const run = queue.then(async () => {
    if (!prompt) return null;
    try {
      return await prompt({ retry });
    } catch {
      return null;
    }
  });
  // Keep the chain alive even if one link rejects.
  queue = run.catch(() => null);
  return run;
}
