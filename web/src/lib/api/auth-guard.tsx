import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, useLocation } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { LoadingBlock } from "@/components/patterns/loading";
import {
  AuthContext,
  useAuth,
  type AuthContextValue,
  type AuthPhase,
  type AuthProviderProps,
} from "@/lib/api/auth-context";
import { hoservaClient, type components } from "@/lib/api/client";
import {
  inferOnboardingCompleteIfNeeded,
  isOnboardingComplete,
} from "@/lib/api/onboarding";

type User = components["schemas"]["User"];

async function loadAuthState(): Promise<{
  adminExists: boolean;
  user: User | null;
  failed: boolean;
}> {
  try {
    const [setupResult, sessionResult] = await Promise.all([
      hoservaClient.GET("/setup/status"),
      hoservaClient.GET("/auth/session"),
    ]);

    if (!setupResult.response) {
      return { adminExists: false, user: null, failed: true };
    }
    if (setupResult.error && setupResult.data === undefined) {
      return { adminExists: false, user: null, failed: true };
    }
    if (!sessionResult.response) {
      return { adminExists: setupResult.data?.adminExists ?? false, user: null, failed: true };
    }

    const adminExists = setupResult.data?.adminExists ?? false;
    const user = sessionResult.response.ok ? (sessionResult.data ?? null) : null;
    inferOnboardingCompleteIfNeeded(adminExists, user !== null);
    return { adminExists, user, failed: false };
  } catch {
    return { adminExists: false, user: null, failed: true };
  }
}

function resolvePhase(adminExists: boolean, user: User | null): AuthPhase {
  if (!adminExists) {
    return "welcome";
  }
  if (!user) {
    return "login";
  }
  if (!isOnboardingComplete()) {
    return "welcome";
  }
  return "authenticated";
}

export function AuthProvider({ children }: AuthProviderProps): React.ReactElement {
  const [adminExists, setAdminExists] = useState(false);
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  const [probeFailed, setProbeFailed] = useState(false);

  const refresh = useCallback(async (): Promise<void> => {
    const next = await loadAuthState();
    if (next.failed) {
      return;
    }
    setProbeFailed(false);
    setAdminExists(next.adminExists);
    setUser(next.user);
  }, []);

  const acceptSession = useCallback((next: User): void => {
    setProbeFailed(false);
    setAdminExists(true);
    setUser(next);
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    loadAuthState()
      .then((next) => {
        if (controller.signal.aborted) {
          return;
        }
        if (next.failed) {
          setProbeFailed(true);
          return;
        }
        setAdminExists(next.adminExists);
        setUser(next.user);
      })
      .finally(() => {
        if (!controller.signal.aborted) {
          setLoading(false);
        }
      });
    return () => controller.abort();
  }, []);

  const phase: AuthPhase = loading
    ? "loading"
    : probeFailed
      ? "error"
      : resolvePhase(adminExists, user);
  const value = useMemo<AuthContextValue>(
    () => ({ phase, user, adminExists, refresh, acceptSession }),
    [phase, user, adminExists, refresh, acceptSession],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function AuthGate({ children }: { children: ReactNode }): React.ReactElement {
  const { phase } = useAuth();
  const location = useLocation();
  const { t } = useTranslation();

  if (phase === "loading") {
    return (
      <div className="flex min-h-svh items-center justify-center p-4">
        <LoadingBlock />
      </div>
    );
  }

  if (phase === "error") {
    return (
      <div className="flex min-h-svh items-center justify-center p-4">
        <Banner tone="error" title={t("auth.loadFailed")} />
      </div>
    );
  }

  const isWelcome = location.pathname === "/welcome";
  const isLogin = location.pathname === "/login";

  if (phase === "welcome" && !isWelcome) {
    return <Navigate to="/welcome" replace state={{ from: location.pathname }} />;
  }

  if (phase === "login" && !isLogin) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }

  if (phase === "authenticated" && (isWelcome || isLogin)) {
    const from =
      typeof location.state === "object" &&
      location.state !== null &&
      "from" in location.state &&
      typeof location.state.from === "string"
        ? location.state.from
        : "/";
    return <Navigate to={from} replace />;
  }

  return <>{children}</>;
}

export function MinimalAuthLayout({ children }: { children: ReactNode }): React.ReactElement {
  return (
    <div className="flex min-h-svh flex-col items-center justify-center bg-background p-4">
      <div className="mb-8 text-center">
        <h1 className="font-heading text-2xl font-semibold">Hoserva</h1>
      </div>
      <div className="w-full max-w-2xl">{children}</div>
    </div>
  );
}
