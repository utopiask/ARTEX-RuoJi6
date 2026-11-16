"use client";

// 资产导入导出弹窗（实施方案 §1 四步闭环 + §10 导出范围）。两个入口共用：
// 全局资产页（不预设归属）与任务资产标签页（预设任务关联）。预览只读、
// 提交绑定 plan_hash、补全默认关闭——与后端合同一一对应。

import * as React from "react";

import { DownloadIcon, FileUpIcon, Loader2Icon, UploadIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { useUITranslation } from "@/i18n/use-ui-translation";
import { api, getToken } from "@/lib/api";
import type {
  AssetExportRequest,
  AssetImportBatchResponse,
  AssetImportDiff,
  AssetImportOptions,
  AssetImportPreviewResponse,
  AssetImportReceipt,
  Company,
  Task,
} from "@/lib/types";

const ACTION_TONE: Record<string, string> = {
  create: "bg-emerald-100 text-emerald-700",
  fill: "bg-blue-100 text-blue-700",
  overwrite: "bg-amber-100 text-amber-700",
  unchanged: "bg-muted text-muted-foreground",
  conflict: "bg-red-100 text-red-700",
  invalid: "bg-red-100 text-red-700",
};

function ActionBadge({ action, label }: { action: string; label: string }) {
  return (
    <span
      className={`inline-block rounded px-1.5 py-0.5 text-[10px] font-semibold leading-none ${ACTION_TONE[action] ?? "bg-muted text-muted-foreground"}`}
    >
      {label}
    </span>
  );
}

function rowObjectRef(row: AssetImportBatchResponse["rows"][number]["row"]): string {
  switch (row.type) {
    case "root_domain":
    case "subdomain":
      return row.domain || "#";
    case "ip":
      return row.ip || "#";
    case "app":
      return row.bundle_id ? `${row.app_name} (${row.bundle_id})` : row.app_name || "#";
    case "service":
      return row.url || `${row.domain || row.ip}:${row.port ?? ""}/${row.service_name ?? ""}`;
    case "endpoint":
      return `${row.method ?? ""} ${row.url ?? ""}`.trim() || "#";
    default:
      return row.domain || row.ip || row.url || "#";
  }
}

function diffText(d: AssetImportDiff): string {
  const arrow = d.old !== undefined || d.new !== undefined ? `${d.old ?? ""}→${d.new ?? ""}` : "";
  return [d.field, arrow, d.note].filter(Boolean).join(" ");
}

export function AssetImportDialog({
  open,
  onOpenChange,
  onImported,
  presetTaskId,
  presetTaskLabel,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onImported: () => void;
  /** 任务页入口：预选该任务（不改归属规则，仅建立关联）。 */
  presetTaskId?: number;
  presetTaskLabel?: string;
}) {
  const { ui } = useUITranslation();
  const [step, setStep] = React.useState<"source" | "preview" | "done">("source");
  const [file, setFile] = React.useState<File | null>(null);
  const [text, setText] = React.useState("");
  const [conflict, setConflict] = React.useState<"fill" | "overwrite">("fill");
  const [companyId, setCompanyId] = React.useState("0");
  const [taskId, setTaskId] = React.useState(presetTaskId ? String(presetTaskId) : "0");
  const [companyPolicy, setCompanyPolicy] = React.useState<"exclude" | "keep">("exclude");
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [busy, setBusy] = React.useState(false);
  const [loaded, setLoaded] = React.useState<{
    preview: AssetImportPreviewResponse;
    batch: AssetImportBatchResponse;
    signature: string;
    generation: number;
  } | null>(null);
  const generation = React.useRef(0);
  const pageSequence = React.useRef(0);
  const committing = React.useRef(false);
  const [actionFilter, setActionFilter] = React.useState("");
  const [receipt, setReceipt] = React.useState<AssetImportReceipt | null>(null);
  const [probeChecked, setProbeChecked] = React.useState(false);

  const invalidate = () => {
    generation.current++;
    pageSequence.current++;
    committing.current = false;
    setLoaded(null);
    setStep("source");
    setActionFilter("");
    setBusy(false);
  };

  React.useEffect(() => {
    invalidate();
    setTaskId(presetTaskId ? String(presetTaskId) : "0");
    if (!open) return;
    let active = true;
    setFile(null);
    setText("");
    setReceipt(null);
    setActionFilter("");
    setProbeChecked(false);
    setBusy(false);
    api
      .companies()
      .then((r) => {
        if (active) setCompanies(r);
      })
      .catch(() => {
        if (active) setCompanies([]);
      });
    if (!presetTaskId) {
      api
        .tasks()
        .then((r) => {
          if (active) setTasks(r.tasks);
        })
        .catch(() => {
          if (active) setTasks([]);
        });
    }
    return () => {
      active = false;
      generation.current++;
      pageSequence.current++;
    };
  }, [open, presetTaskId]);

  const options: AssetImportOptions = {
    conflict,
    company_id: Number(companyId) > 0 ? Number(companyId) : undefined,
    task_id: Number(taskId) > 0 ? Number(taskId) : undefined,
    company_policy: companyPolicy,
  };

  const signature = JSON.stringify([text, conflict, companyId, taskId, companyPolicy, presetTaskId]);
  const preview = loaded?.preview ?? null;
  const batch = loaded?.batch ?? null;
  const matches = !!(
    open &&
    loaded &&
    loaded.generation === generation.current &&
    loaded.signature === signature &&
    preview?.plan_hash &&
    preview.batch_id === batch?.batch_id &&
    preview.plan_hash === batch.plan_hash &&
    batch.status === "ready"
  );
  const actionable =
    (preview?.summary.create ?? 0) +
      (preview?.summary.fill ?? 0) +
      (preview?.summary.overwrite ?? 0) +
      (options.task_id ? (preview?.summary.unchanged ?? 0) : 0) >
    0;

  const runPreview = async () => {
    invalidate();
    if (!file && text.trim() === "") {
      toast.error(ui("assetTransfer.source"));
      return;
    }
    const current = generation.current;
    setBusy(true);
    try {
      const result = await api.assetImportPreview({ file: file ?? undefined, text: text.trim() || undefined, options });
      if (current !== generation.current) return;
      const details = await api.assetImportBatch(result.batch_id, { offset: 0, limit: 100 });
      if (current !== generation.current) return;
      if (
        !result.plan_hash ||
        details.batch_id !== result.batch_id ||
        details.plan_hash !== result.plan_hash ||
        details.status !== "ready"
      )
        throw new Error(ui("assetTransfer.stale"));
      setLoaded({ preview: result, batch: details, signature, generation: current });
      setStep("preview");
    } catch (e) {
      if (current === generation.current) toast.error(String((e as Error)?.message ?? e));
    } finally {
      if (current === generation.current) setBusy(false);
    }
  };

  const loadBatchPage = async (id: number, action: string, offset = 0) => {
    if (!matches || committing.current) return;
    const current = generation.current;
    const page = ++pageSequence.current;
    setBusy(true);
    try {
      const result = await api.assetImportBatch(id, { action: action || undefined, offset, limit: 100 });
      if (current !== generation.current || page !== pageSequence.current) return;
      if (result.batch_id !== id || result.plan_hash !== preview?.plan_hash || result.status !== "ready") {
        throw new Error(ui("assetTransfer.stale"));
      }
      setLoaded((old) => (old && old.generation === current ? { ...old, batch: result } : old));
      setActionFilter(action);
    } catch (e) {
      if (current === generation.current && page === pageSequence.current) {
        invalidate();
        toast.error(String((e as Error)?.message ?? e));
      }
    } finally {
      if (current === generation.current && page === pageSequence.current) setBusy(false);
    }
  };

  const commit = async () => {
    if (!matches || !actionable || !preview || busy || committing.current || loaded?.generation !== generation.current)
      return;
    const current = generation.current;
    committing.current = true;
    setBusy(true);
    try {
      const result = await api.assetImportCommit(preview.batch_id, preview.plan_hash);
      if (current !== generation.current) return;
      setReceipt(result.receipt);
      setStep("done");
      toast.success(
        ui("assetTransfer.importDone", {
          v0: result.receipt.summary.create,
          v1: result.receipt.summary.fill,
          v2: result.receipt.summary.overwrite,
          v3: result.receipt.summary.unchanged,
        }),
      );
      if (probeChecked) await runProbe(preview.batch_id);
      if (current === generation.current) onImported();
    } catch (e) {
      if (current === generation.current) toast.error(String((e as Error)?.message ?? e));
    } finally {
      if (current === generation.current) {
        committing.current = false;
        setBusy(false);
      }
    }
  };

  const runProbe = async (batchId: number) => {
    try {
      const result = await api.assetImportProbe(batchId);
      toast.info(ui("assetTransfer.probeQueued", { v0: result.queued, v1: result.not_queued, v2: result.skipped }));
      if (result.note) toast.info(result.note);
    } catch (e) {
      toast.error(String((e as Error)?.message ?? e));
    }
  };

  const downloadErrors = () => {
    if (!preview) return;
    const url = api.assetImportErrorsURL(preview.batch_id);
    if (!url) {
      toast.info(ui("assetTransfer.errorsDemo"));
      return;
    }
    const token = getToken();
    void apiFetchDownload(url, token).catch((e) => toast.error(String((e as Error)?.message ?? e)));
  };

  const summary = preview?.summary;

  return (
    <Dialog
      open={open}
      onOpenChange={(value) => {
        if (!value) invalidate();
        onOpenChange(value);
      }}
    >
      <DialogContent className="max-h-[90vh] max-w-3xl overflow-hidden">
        <DialogHeader>
          <DialogTitle>{ui("assetTransfer.importTitle")}</DialogTitle>
          <DialogDescription>
            {step === "source"
              ? ui("assetTransfer.source")
              : step === "preview"
                ? ui("assetTransfer.resPreview")
                : ui("assetTransfer.result")}
          </DialogDescription>
        </DialogHeader>

        {step === "source" ? (
          <ScrollArea className="max-h-[62vh] pr-3">
            <div className="flex flex-col gap-4">
              <Tabs defaultValue="file">
                <TabsList>
                  <TabsTrigger value="file">
                    <FileUpIcon data-icon="inline-start" />
                    {ui("assetTransfer.file")}
                  </TabsTrigger>
                  <TabsTrigger value="paste">{ui("assetTransfer.paste")}</TabsTrigger>
                </TabsList>
                <TabsContent value="file" className="mt-2 flex flex-col gap-2">
                  <Input
                    type="file"
                    accept=".xlsx,.csv,.json,.txt"
                    onChange={(e) => {
                      invalidate();
                      setFile(e.target.files?.[0] ?? null);
                    }}
                  />
                  <Button
                    variant="outline"
                    size="sm"
                    className="w-fit"
                    onClick={() => void api.assetImportTemplate("xlsx")}
                  >
                    <DownloadIcon data-icon="inline-start" />
                    {ui("assetTransfer.template")}
                  </Button>
                </TabsContent>
                <TabsContent value="paste" className="mt-2">
                  <Textarea
                    rows={5}
                    placeholder={
                      "example.com\nwww.example.com\n203.0.113.10\nhttps://api.example.com/v1\nGET https://api.example.com/v1/users"
                    }
                    value={text}
                    onChange={(e) => {
                      invalidate();
                      setText(e.target.value);
                    }}
                  />
                </TabsContent>
              </Tabs>

              <FieldGroup>
                <Field>
                  <FieldLabel>{ui("assetTransfer.strategy")}</FieldLabel>
                  <RadioGroup
                    value={conflict}
                    onValueChange={(v) => {
                      invalidate();
                      setConflict(v as "fill" | "overwrite");
                    }}
                  >
                    <div className="flex items-center gap-2">
                      <RadioGroupItem value="fill" id="import-fill" />
                      <Label htmlFor="import-fill">{ui("assetTransfer.fill")}</Label>
                    </div>
                    <div className="flex items-center gap-2">
                      <RadioGroupItem value="overwrite" id="import-overwrite" />
                      <Label htmlFor="import-overwrite">{ui("assetTransfer.overwrite")}</Label>
                    </div>
                  </RadioGroup>
                </Field>
                <div className="grid grid-cols-2 gap-3">
                  <Field>
                    <FieldLabel>{ui("assetTransfer.company")}</FieldLabel>
                    <Select
                      value={companyId}
                      onValueChange={(v) => {
                        invalidate();
                        setCompanyId(v);
                      }}
                    >
                      <SelectTrigger size="sm">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="0">{ui("assetTransfer.companyNone")}</SelectItem>
                        {companies.map((c) => (
                          <SelectItem key={c.id} value={String(c.id)}>
                            {c.name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                  {Number(companyId) > 0 ? (
                    <Field>
                      <FieldLabel>{ui("assetTransfer.companyConflict")}</FieldLabel>
                      <Select
                        value={companyPolicy}
                        onValueChange={(v) => {
                          invalidate();
                          setCompanyPolicy(v as "exclude" | "keep");
                        }}
                      >
                        <SelectTrigger size="sm">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="exclude">{ui("assetTransfer.companyExclude")}</SelectItem>
                          <SelectItem value="keep">{ui("assetTransfer.companyKeep")}</SelectItem>
                        </SelectContent>
                      </Select>
                    </Field>
                  ) : null}
                </div>
                {presetTaskId ? null : (
                  <Field>
                    <FieldLabel>{ui("assetTransfer.task")}</FieldLabel>
                    <Select
                      value={taskId}
                      onValueChange={(v) => {
                        invalidate();
                        setTaskId(v);
                      }}
                    >
                      <SelectTrigger size="sm">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="0">{ui("assetTransfer.taskNone")}</SelectItem>
                        {tasks.map((t) => (
                          <SelectItem key={t.id} value={String(t.numeric_id ?? t.id)}>
                            {t.name || t.description?.slice(0, 40) || t.id}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                )}
                {presetTaskId ? (
                  <div className="text-muted-foreground text-xs">
                    {ui("assetTransfer.task")}: {presetTaskLabel ?? `#${presetTaskId}`} ·{" "}
                    {ui("assetTransfer.taskScopeHint")}
                  </div>
                ) : null}
              </FieldGroup>
            </div>
          </ScrollArea>
        ) : null}

        {step === "preview" && preview && batch ? (
          <div className="flex max-h-[62vh] flex-col gap-3 overflow-hidden">
            <div className="flex flex-wrap items-center gap-2 text-xs">
              {(["create", "fill", "overwrite", "unchanged", "conflict", "invalid"] as const).map((key) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => void loadBatchPage(preview.batch_id, actionFilter === key ? "" : key)}
                >
                  <span className={actionFilter === key ? "underline" : ""}>
                    <ActionBadge action={key} label={ui(`assetTransfer.action.${key}`)} /> {summary?.[key] ?? 0}
                  </span>
                </button>
              ))}
              <Button variant="outline" size="sm" onClick={downloadErrors}>
                <DownloadIcon data-icon="inline-start" />
                {ui("assetTransfer.downloadErrors")}
              </Button>
              <Button variant="ghost" size="sm" className="ml-auto" disabled={busy} onClick={invalidate}>
                {ui("assetTransfer.rePreview")}
              </Button>
            </div>
            <ScrollArea className="min-h-0 flex-1">
              <Table>
                <TableHeader className="sticky top-0 z-10 bg-card">
                  <TableRow>
                    <TableHead className="w-24">{ui("assetTransfer.col.location")}</TableHead>
                    <TableHead className="w-20">{ui("assetTransfer.col.type")}</TableHead>
                    <TableHead>{ui("assetTransfer.col.object")}</TableHead>
                    <TableHead className="w-20">{ui("assetTransfer.col.action")}</TableHead>
                    <TableHead>{ui("assetTransfer.col.diffs")}</TableHead>
                    <TableHead>{ui("assetTransfer.col.note")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {batch.rows.map((row) => (
                    <TableRow key={`${row.row.sheet}-${row.row.line}-${row.index}`}>
                      <TableCell className="text-muted-foreground font-mono text-xs">
                        {row.row.sheet ? `${row.row.sheet}#${row.row.line}` : `#${row.row.line}`}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{row.row.type}</TableCell>
                      <TableCell className="max-w-56 truncate font-mono text-xs" title={rowObjectRef(row.row)}>
                        {rowObjectRef(row.row)}
                      </TableCell>
                      <TableCell>
                        <ActionBadge action={row.action} label={ui(`assetTransfer.action.${row.action}`)} />
                      </TableCell>
                      <TableCell className="text-xs">
                        {(row.diffs ?? []).map((d, i) => (
                          <div key={i} className="[overflow-wrap:anywhere]">
                            {diffText(d)}
                          </div>
                        ))}
                      </TableCell>
                      <TableCell className="text-xs">
                        {row.reason ? <div className="[overflow-wrap:anywhere]">{row.reason}</div> : null}
                        {row.shared_with_tasks?.length ? (
                          <div className="text-amber-600">
                            {ui("assetTransfer.sharedTasks", { v0: row.shared_with_tasks.join(", ") })}
                          </div>
                        ) : null}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </ScrollArea>
            {batch.rows_total > batch.limit ? (
              <div className="text-muted-foreground flex items-center gap-2 text-xs">
                {batch.offset + 1}–{Math.min(batch.offset + batch.rows.length, batch.rows_total)} / {batch.rows_total}
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batch.offset <= 0}
                  onClick={() =>
                    void loadBatchPage(preview.batch_id, actionFilter, Math.max(0, batch.offset - batch.limit))
                  }
                >
                  {ui("common.previous")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batch.offset + batch.limit >= batch.rows_total}
                  onClick={() => void loadBatchPage(preview.batch_id, actionFilter, batch.offset + batch.limit)}
                >
                  {ui("common.next")}
                </Button>
              </div>
            ) : null}
          </div>
        ) : null}

        {step === "done" && receipt ? (
          <div className="flex max-h-[62vh] flex-col gap-3">
            <div className="flex flex-wrap gap-2 text-xs">
              <Badge variant="outline">
                {ui("assetTransfer.action.create")} {receipt.summary.create}
              </Badge>
              <Badge variant="outline">
                {ui("assetTransfer.action.fill")} {receipt.summary.fill}
              </Badge>
              <Badge variant="outline">
                {ui("assetTransfer.action.overwrite")} {receipt.summary.overwrite}
              </Badge>
              <Badge variant="outline">
                {ui("assetTransfer.action.unchanged")} {receipt.summary.unchanged}
              </Badge>
              {receipt.attached > 0 ? (
                <Badge variant="outline">
                  {ui("assetTransfer.task")} +{receipt.attached}
                </Badge>
              ) : null}
            </div>
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <Checkbox
                  id="import-probe"
                  checked={probeChecked}
                  onCheckedChange={(v) => setProbeChecked(v === true)}
                />
                <Label htmlFor="import-probe">{ui("assetTransfer.probe")}</Label>
              </div>
              <div className="flex gap-2">
                <Button variant="outline" size="sm" onClick={downloadErrors}>
                  <DownloadIcon data-icon="inline-start" />
                  {ui("assetTransfer.downloadErrors")}
                </Button>
                {probeChecked ? (
                  <Button variant="outline" size="sm" onClick={() => void runProbe(receipt.batch_id)}>
                    {ui("assetTransfer.probeRun")}
                  </Button>
                ) : null}
              </div>
            </div>
          </div>
        ) : null}

        <DialogFooter>
          {step === "source" ? (
            <Button onClick={() => void runPreview()} disabled={busy}>
              {busy ? (
                <Loader2Icon data-icon="inline-start" className="animate-spin" />
              ) : (
                <UploadIcon data-icon="inline-start" />
              )}
              {ui("assetTransfer.preview")}
            </Button>
          ) : null}
          {step === "preview" ? (
            <Button onClick={() => void commit()} disabled={busy || !matches || !actionable}>
              {busy ? <Loader2Icon data-icon="inline-start" className="animate-spin" /> : null}
              {ui("assetTransfer.confirmImport")}
            </Button>
          ) : null}
          <Button
            variant="outline"
            onClick={() => {
              invalidate();
              onOpenChange(false);
            }}
          >
            {ui("common.close")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function AssetExportDialog({
  open,
  onOpenChange,
  selectedIds,
  filter,
  taskLabel,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 勾选导出的资产 ID；空则该选项不可用。 */
  selectedIds: number[];
  /** 当前筛选（type/company/task/dsl）；任务页入口传 { task_id }。 */
  filter?: AssetExportRequest["filter"];
  taskLabel?: string;
}) {
  const { ui } = useUITranslation();
  const [scope, setScope] = React.useState<"selected" | "filtered" | "all">(
    selectedIds.length > 0 ? "selected" : "filtered",
  );
  const [format, setFormat] = React.useState<"xlsx" | "csv" | "json">("xlsx");
  const [includeSensitive, setIncludeSensitive] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    setScope(selectedIds.length > 0 ? "selected" : "filtered");
    setFormat("xlsx");
    setIncludeSensitive(false);
  }, [open, selectedIds.length]);

  const run = async () => {
    if (scope === "selected" && selectedIds.length === 0) return;
    setBusy(true);
    try {
      await api.assetExport({
        scope,
        format,
        include_sensitive: includeSensitive,
        ids: scope === "selected" ? selectedIds : undefined,
        filter: scope === "filtered" ? (filter ?? {}) : undefined,
      });
      toast.success(ui("assetTransfer.export"));
    } catch (e) {
      toast.error(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>{ui("assetTransfer.exportTitle")}</DialogTitle>
          {taskLabel ? <DialogDescription>{ui("assetTransfer.taskScopeHint")}</DialogDescription> : null}
        </DialogHeader>
        <div className="flex flex-col gap-4">
          <Field>
            <FieldLabel>{ui("assetTransfer.export.scope")}</FieldLabel>
            <RadioGroup value={scope} onValueChange={(v) => setScope(v as typeof scope)}>
              <div className="flex items-center gap-2">
                <RadioGroupItem value="selected" id="exp-selected" disabled={selectedIds.length === 0} />
                <Label htmlFor="exp-selected">{ui("assetTransfer.export.selected", { v0: selectedIds.length })}</Label>
              </div>
              <div className="flex items-center gap-2">
                <RadioGroupItem value="filtered" id="exp-filtered" disabled={!filter} />
                <Label htmlFor="exp-filtered">{ui("assetTransfer.export.filtered")}</Label>
              </div>
              <div className="flex items-center gap-2">
                <RadioGroupItem value="all" id="exp-all" />
                <Label htmlFor="exp-all">{ui("assetTransfer.export.all")}</Label>
              </div>
            </RadioGroup>
          </Field>
          <Field>
            <FieldLabel>{ui("assetTransfer.export.format")}</FieldLabel>
            <Select value={format} onValueChange={(v) => setFormat(v as typeof format)}>
              <SelectTrigger size="sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="xlsx">XLSX</SelectItem>
                <SelectItem value="csv">CSV</SelectItem>
                <SelectItem value="json">JSON</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          <div className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              <Checkbox
                id="exp-sensitive"
                checked={includeSensitive}
                onCheckedChange={(v) => setIncludeSensitive(v === true)}
              />
              <Label htmlFor="exp-sensitive">{ui("assetTransfer.export.sensitive")}</Label>
            </div>
            {includeSensitive ? (
              <p className="text-amber-600 text-xs">{ui("assetTransfer.export.sensitiveHint")}</p>
            ) : null}
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => void run()} disabled={busy}>
            {busy ? (
              <Loader2Icon data-icon="inline-start" className="animate-spin" />
            ) : (
              <DownloadIcon data-icon="inline-start" />
            )}
            {ui("assetTransfer.export")}
          </Button>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {ui("common.cancel")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** 认证下载错误清单（GET，浏览器直接带不了 Authorization 时兜底为 blob）。 */
async function apiFetchDownload(url: string, token: string | null) {
  const r = await fetch(url, { headers: token ? { Authorization: `Bearer ${token}` } : {} });
  if (!r.ok) {
    toast.error(`下载失败: ${r.status}`);
    return;
  }
  const blob = await r.blob();
  const objUrl = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = objUrl;
  a.download = `artex-导入错误清单.csv`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(objUrl);
}
