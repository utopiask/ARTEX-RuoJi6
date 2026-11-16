"use client";

import * as React from "react";

import { HardDriveIcon } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useUITranslation } from "@/i18n/use-ui-translation";
import { api } from "@/lib/api";
import type { TrafficStorage } from "@/lib/types";

function formatBytes(bytes: number) {
  if (bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

export function TrafficStorageUsage({ reloadTick }: { reloadTick: number }) {
  const { ui, i18n } = useUITranslation();
  const [storage, setStorage] = React.useState<TrafficStorage | null>(null);
  const [failed, setFailed] = React.useState(false);

  // biome-ignore lint/correctness/useExhaustiveDependencies: deletion triggers a fresh cached snapshot.
  React.useEffect(() => {
    let alive = true;
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      let delay = 60_000;
      try {
        if (document.visibilityState !== "hidden") {
          const result = await api.trafficStorage();
          if (!alive) return;
          setStorage(result);
          setFailed(false);
          if (result.refreshing) delay = 5_000;
        }
      } catch {
        if (alive) setFailed(true);
      }
      if (alive) timer = setTimeout(() => void load(), delay);
    };
    void load();
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [reloadTick]);

  let label = ui("traffic.storage.loading");
  if (storage?.ready) label = ui("traffic.storage.approx", { size: formatBytes(storage.total_bytes) });
  else if (failed || storage?.error) label = ui("traffic.storage.unavailable");
  else if (storage && !storage.refreshing) label = ui("traffic.storage.empty");

  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs text-muted-foreground hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          aria-label={ui("traffic.storage.aria", { label })}
        >
          <HardDriveIcon className="size-3.5 shrink-0" />
          <span>
            {ui("traffic.storage.label")} <span className="tabular-nums">{label}</span>
          </span>
          {storage?.ready && (failed || storage.stale || storage.refreshing) && (
            <span>{ui(storage.refreshing ? "traffic.storage.updating" : "traffic.storage.stale")}</span>
          )}
        </button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 max-w-[calc(100vw-2rem)] space-y-3 text-xs">
        <p className="font-medium text-foreground">{ui("traffic.storage.title")}</p>
        <p className="text-muted-foreground">{ui("traffic.storage.scope")}</p>
        {storage?.ready && (
          <dl className="space-y-1 tabular-nums">
            <div className="flex justify-between gap-4">
              <dt>{ui("traffic.storage.index")}</dt>
              <dd>{formatBytes(storage.index_bytes)}</dd>
            </div>
            <div className="flex justify-between gap-4">
              <dt>{ui("traffic.storage.external")}</dt>
              <dd>{formatBytes(storage.external_bytes)}</dd>
            </div>
            <div className="flex justify-between gap-4 font-medium">
              <dt>{ui("traffic.storage.total")}</dt>
              <dd>{formatBytes(storage.total_bytes)}</dd>
            </div>
          </dl>
        )}
        {storage?.updated_at && (
          <p className="text-muted-foreground">
            {ui("traffic.storage.updated", {
              time: new Date(storage.updated_at).toLocaleString(i18n.resolvedLanguage || i18n.language),
            })}
          </p>
        )}
        {(failed || storage?.error) && (
          <p role="status" className="text-amber-600 dark:text-amber-400">
            {ui(storage?.ready ? "traffic.storage.failedPrevious" : "traffic.storage.failed")}
          </p>
        )}
        <p className="text-muted-foreground">{ui("traffic.storage.delay")}</p>
      </PopoverContent>
    </Popover>
  );
}
