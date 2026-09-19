import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { ToastProvider } from "@/components/ui/toast";
import "./lib/i18n";
import "./index.css";
import { App } from "./App";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ToastProvider>
      <App />
    </ToastProvider>
  </StrictMode>,
);
