import { resolveDeployment } from "./deployment";

// The deployment values for this build (see config/deployment.ts).
export const deployment = resolveDeployment(import.meta.env);
