import { createContext, useContext, type ReactNode } from "react";

import type { components } from "@/lib/api/client";

type User = components["schemas"]["User"];

export type AuthPhase = "loading" | "welcome" | "login" | "authenticated";

export interface AuthContextValue {
  phase: AuthPhase;
  user: User | null;
  adminExists: boolean;
  refresh: () => Promise<void>;
}

export const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) {
    throw new Error("useAuth must be used within AuthProvider");
  }
  return value;
}

export type AuthProviderProps = { children: ReactNode };
