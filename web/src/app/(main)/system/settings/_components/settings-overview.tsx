"use client";

import * as React from "react";

import { ArrowUpIcon, SearchIcon, SlidersHorizontalIcon, XIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { matchesSettingsQuery } from "@/i18n/settings-search";
import { useLiveTranslation } from "@/i18n/use-live-translation";
import { cn } from "@/lib/utils";

export type SettingsSaveMode = "instant" | "form" | "browser" | "experimental" | "confirm" | "mixed";

export type SettingsGroupSpec = {
  id: string;
  titleKey: string;
  descriptionKey: string;
  cards: {
    id: string;
    keywords: string;
    content: React.ReactNode;
    wide?: boolean;
    saveMode?: SettingsSaveMode;
  }[];
};

type SettingsOverviewProps = {
  groups: SettingsGroupSpec[];
  query: string;
  onQueryChange: (value: string) => void;
};

export function SettingsOverview({ groups, query, onQueryChange }: SettingsOverviewProps) {
  const { t, i18n } = useLiveTranslation();
  const [comfortable, setComfortable] = React.useState(false);
  const searching = Boolean(query.trim());
  const matches = (keywords: string) => matchesSettingsQuery(i18n, keywords, query);
  const total = groups.reduce((count, group) => count + group.cards.length, 0);
  const matched = groups.reduce((count, group) => count + group.cards.filter((card) => matches(card.keywords)).length, 0);

  return (
    <div
      id="settings-overview"
      className={cn(
        "flex min-w-0 flex-1 flex-col gap-5",
        !comfortable &&
          "[&_[data-slot=card]]:gap-4 [&_[data-slot=card]]:py-4 [&_[data-slot=card-header]]:px-4 [&_[data-slot=card-content]]:gap-4 [&_[data-slot=card-content]]:px-4 [&_[data-slot=card-footer]]:px-4 [&_[data-slot=card-description]]:text-xs [&_[data-slot=card-description]]:leading-relaxed",
      )}
      data-settings-density={comfortable ? "comfortable" : "compact"}
    >
      <div className="flex flex-col justify-between gap-4 md:flex-row md:items-start">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">{t("settings.title")}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t("settings.overview.subtitle")}</p>
        </div>
        <div className="relative w-full md:max-w-sm">
          <SearchIcon className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(event) => onQueryChange(event.target.value)}
            placeholder={t("settings.searchPlaceholder")}
            className="pr-9 pl-9"
            aria-label={t("settings.searchLabel")}
          />
          {searching ? (
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              className="absolute top-1/2 right-1 -translate-y-1/2"
              aria-label={t("settings.clearSearch")}
              onClick={() => onQueryChange("")}
            >
              <XIcon className="size-3.5" />
            </Button>
          ) : null}
        </div>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3 border-b pb-3">
        {searching ? (
          <p className="text-xs text-muted-foreground" role="status">
            {t("settings.matchingSettings", { query: query.trim() })}
          </p>
        ) : (
          <nav className="flex flex-wrap items-center gap-x-6 gap-y-2" aria-label={t("settings.overview.groupNavigation")}>
            {groups.map((group) => (
              <a
                key={group.id}
                href={`#settings-group-${group.id}`}
                className="text-xs text-muted-foreground underline-offset-4 hover:text-foreground hover:underline focus-visible:rounded-sm focus-visible:outline-2 focus-visible:outline-offset-4"
              >
                {t(group.titleKey)}
              </a>
            ))}
          </nav>
        )}
        <div className="flex items-center gap-3">
          <span className="text-xs text-muted-foreground" aria-live="polite">
            {searching ? t("settings.overview.matches", { matched, total }) : t("settings.overview.total", { count: total })}
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 gap-1.5 text-xs"
            aria-pressed={comfortable}
            onClick={() => setComfortable((value) => !value)}
          >
            <SlidersHorizontalIcon className="size-3.5" />
            {t(comfortable ? "settings.overview.comfortable" : "settings.overview.compact")}
          </Button>
        </div>
      </div>

      {groups.map((group) => {
        const visible = group.cards.some((card) => matches(card.keywords));
        return (
          <section
            key={group.id}
            id={`settings-group-${group.id}`}
            aria-labelledby={`settings-heading-${group.id}`}
            hidden={!visible}
            className={cn("scroll-mt-6 space-y-3", !visible && "hidden")}
          >
            <div className="flex items-center gap-3">
              <h2 id={`settings-heading-${group.id}`} className="text-sm font-semibold">
                {t(group.titleKey)}
              </h2>
              <p className="hidden text-xs text-muted-foreground sm:block">{t(group.descriptionKey)}</p>
              <a
                href="#settings-overview"
                className="ml-auto rounded-sm p-1 text-muted-foreground hover:text-foreground focus-visible:outline-2"
                aria-label={t("settings.overview.backToTop")}
              >
                <ArrowUpIcon className="size-3.5" />
              </a>
            </div>
            <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
              {group.cards.map((card) => {
                const visible = matches(card.keywords);
                return (
                  <div
                    key={card.id}
                    id={`settings-card-${card.id}`}
                    data-settings-card={card.id}
                    hidden={!visible}
                    className={cn(
                      "relative min-w-0 scroll-mt-6 [&>[data-slot=card]]:mb-0",
                      card.wide && "lg:col-span-2",
                      card.saveMode && "[&>[data-slot=card]_[data-slot=card-title]]:pr-24",
                      !visible && "hidden",
                    )}
                  >
                    {card.saveMode ? (
                      <span className="pointer-events-none absolute top-4 right-4 z-1 rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                        {t(`settings.overview.mode.${card.saveMode}`)}
                      </span>
                    ) : null}
                    {card.content}
                  </div>
                );
              })}
            </div>
          </section>
        );
      })}

      {matched === 0 ? (
        <p role="status" className="rounded-xl border border-dashed px-6 py-10 text-center text-sm text-muted-foreground">
          {t("settings.overview.noResults")}
        </p>
      ) : null}

      <p className="border-t pt-4 text-xs text-muted-foreground">{t("settings.overview.saveHelp")}</p>
    </div>
  );
}
