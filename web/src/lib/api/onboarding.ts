const COMPLETE_KEY = "hoserva.onboardingComplete";
const STEP_KEY = "hoserva.onboardingStep";

export function isOnboardingComplete(): boolean {
  return localStorage.getItem(COMPLETE_KEY) === "true";
}

export function markOnboardingIncomplete(): void {
  localStorage.setItem(COMPLETE_KEY, "false");
}

export function markOnboardingComplete(): void {
  localStorage.setItem(COMPLETE_KEY, "true");
  sessionStorage.removeItem(STEP_KEY);
}

export function inferOnboardingCompleteIfNeeded(adminExists: boolean, hasSession: boolean): void {
  if (!adminExists || !hasSession) {
    return;
  }
  if (localStorage.getItem(COMPLETE_KEY) === null) {
    localStorage.setItem(COMPLETE_KEY, "true");
  }
}

export function readOnboardingStep(): number {
  const raw = sessionStorage.getItem(STEP_KEY);
  if (!raw) {
    return 0;
  }
  const step = Number.parseInt(raw, 10);
  return Number.isFinite(step) && step >= 0 && step <= 3 ? step : 0;
}

export function writeOnboardingStep(step: number): void {
  sessionStorage.setItem(STEP_KEY, String(step));
}
