"use client";

import type { MaturityConfigResponse } from "@multica/core/types";
import { useT } from "../../i18n";

// AIFIRST: metric definitions + known gameability notes (CR-2026-047
// TASK-09). Rendered inside the Method section of the maturity page.

export function MaturityDefinitions({
  cfg,
}: {
  cfg: MaturityConfigResponse | undefined;
}) {
  const { t } = useT("usage");
  if (!cfg) return null;
  return (
    <ul className="space-y-1" data-testid="maturity-definitions">
      {cfg.metrics.map((m) => (
        <li key={m.key}>
          <span className="font-medium">{m.key}</span>
          {" — "}
          <span>
            {t(($) => $.maturity.definition_score, { floor: m.floor, target: m.target })}
          </span>
          {" · "}
          <span>{t(($) => $.maturity.definition_weight, { weight: m.weight })}</span>
          {" · "}
          <span>{t(($) => $.maturity.definition_gameability, { note: m.knownGameability })}</span>
        </li>
      ))}
    </ul>
  );
}
