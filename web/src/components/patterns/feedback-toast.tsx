import { toastManager } from "@/components/ui/toast";

type ToastType = "success" | "error" | "warning" | "info" | "loading";

export function showFeedbackToast({
  type = "info",
  title,
  description,
}: {
  type?: ToastType;
  title: string;
  description?: string;
}): void {
  toastManager.add({
    type,
    title,
    description,
  });
}
