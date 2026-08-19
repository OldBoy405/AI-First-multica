"use client";

import type { MaturityConfigResponse } from "@multica/core/types";

// AIFIRST: metric definitions + known gameability notes (CR-2026-047
// TASK-09). Rendered inside the Method section of the maturity page.

export function MaturityDefinitions({
  cfg,
}: {
  cfg: MaturityConfigResponse | undefined;
}) {
  if (!cfg) return null;
  return (
    <ul className="space-y-1" data-testid="maturity-definitions">
      {cfg.metrics.map((m) => (
        <li key={m.key}>
          <span className="font-medium">{m.key}</span>
          {" — "}
          <span>
            score = clamp(100 × (x − {m.floor}) / ({m.target} − {m.floor}))
          </span>
          {" · "}
          <span>weight {m.weight}</span>
        </li>
      ))}
      <li>
        Known gameability: tokens can be inflated by long prompts; gate
        first-pass can be gamed by pre-reviewing offline; project activity
        counts any task or status event.
      </li>
    </ul>
  );
}
