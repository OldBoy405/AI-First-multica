/**
 * E2E: project chat composer layout alignment (CR-2026-062 TASK-04).
 *
 * Drives the real Team Agent pane inside a seeded project:
 *
 *  (a) wide panel: the composer surface's left/right edges align with the
 *      message column's edges (< 1px tolerance, AC-1) and the surface uses
 *      the same chrome as the ordinary chat composer (AC-2);
 *  (b) 360px narrow panel: no horizontal overflow and no overlap between the
 *      editor / attachment preview / config chips / send-stop button (AC-3);
 *  (c) running stop interaction: after a send the bottom-right stop button
 *      cancels the sent task (AC-5 / SDD §4.3.1);
 *  (d) keyboard send via Mod+Enter (AC-7).
 *
 * Environment contract (SDD §6.9-3): this spec is REAL browser-behavior
 * evidence only when executed against a running frontend + backend
 * (FRONTEND_ORIGIN / PLAYWRIGHT_BASE_URL). `--list` proves parse/
 * discoverability only — it is never evidence. When the environment cannot
 * be established, the implementing node aborts with ENVIRONMENT_MISMATCH and
 * the browser-behavior ACs stay unverified.
 */
import "./env";
import { test, expect, type Page } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";
import type { TestApiClient } from "./fixtures";

const API_BASE =
  process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
const DATABASE_URL =
  process.env.DATABASE_URL ?? "postgres://multica:multica@localhost:5432/multica?sslmode=disable";

async function authedFetch(
  api: TestApiClient,
  path: string,
  init?: RequestInit,
  workspaceSlug?: string,
) {
  const token = api.getToken();
  if (!token) throw new Error("test api client not logged in");
  const headers: Record<string, string> = {
    Authorization: `Bearer ${token}`,
    ...(workspaceSlug ? { "X-Workspace-Slug": workspaceSlug } : {}),
    ...((init?.headers as Record<string, string>) ?? {}),
  };
  return fetch(`${API_BASE}${path}`, { ...init, headers });
}

interface ProjectRow {
  id: string;
}

/**
 * The project chat tab renders TWO composers on one page: the Team Agent
 * composer (ChatInputCore) and the global ChatWindow in the right pane
 * (ChatInput). Both share `data-slot="chat-input-surface"`, so every
 * surface lookup must scope to the project composer subtree (TASK-02
 * contract: composer subtree located via internal testids).
 */
const projectSurface = (page: Page) =>
  page.locator('[data-testid="project-chat-composer"] [data-slot="chat-input-surface"]');

test.describe("Project chat composer (CR-2026-062)", () => {
  let api: TestApiClient;
  let pgClient: pg.Client | null = null;
  let workspaceSlug = "";
  let createdAgentId: string | null = null;
  let createdRuntimeId: string | null = null;
  let createdProjectId: string | null = null;

  test.beforeEach(async () => {
    api = await createTestApi();
    const workspaces = await api.getWorkspaces();
    const ws = workspaces[0]!;
    api.setWorkspaceSlug(ws.slug);
    api.setWorkspaceId(ws.id);
    workspaceSlug = ws.slug;

    pgClient = new pg.Client(DATABASE_URL);
    await pgClient.connect();
    const userRow = await pgClient.query(
      `SELECT id FROM "user" WHERE email = $1 LIMIT 1`,
      [api.getEmail()],
    );
    if (userRow.rows.length === 0) throw new Error("e2e user missing");
    const userId = userRow.rows[0].id as string;

    // Seed a runtime + agent (same shape as chat-attachments.spec.ts). The
    // runtime has no real daemon, so a sent task stays queued — exactly the
    // window the running-stop group needs for the stop affordance.
    const runtimeIns = await pgClient.query(
      `INSERT INTO agent_runtime (
         workspace_id, daemon_id, name, runtime_mode, provider, status,
         device_info, metadata, last_seen_at, owner_id, visibility
       )
       VALUES ($1, NULL, $2, 'cloud', 'hermes', 'online', $3, '{}'::jsonb, now(), $4, 'public')
       RETURNING id`,
      [ws.id, `e2e composer runtime ${Date.now()}`, "E2E composer runtime", userId],
    );
    createdRuntimeId = runtimeIns.rows[0].id as string;

    const agentIns = await pgClient.query(
      `INSERT INTO agent (
         workspace_id, name, description, runtime_mode, runtime_config,
         runtime_id, visibility, max_concurrent_tasks, owner_id
       )
       VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 1, $4)
       RETURNING id`,
      [ws.id, `E2E Composer Agent ${Date.now()}`, createdRuntimeId, userId],
    );
    createdAgentId = agentIns.rows[0].id as string;

    // Create a project and bind the seeded agent as its Team Agent.
    const createRes = await authedFetch(
      api,
      "/api/projects",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title: `E2E Composer Project ${Date.now()}` }),
      },
      workspaceSlug,
    );
    expect(createRes.status).toBe(201);
    const project = (await createRes.json()) as ProjectRow;
    createdProjectId = project.id;

    const updateRes = await authedFetch(
      api,
      `/api/projects/${createdProjectId}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ settings: { team_agent_id: createdAgentId } }),
      },
      workspaceSlug,
    );
    expect(updateRes.status).toBe(200);

    // Seed the runtime's model catalog through the real server machinery:
    // initiate a model-list request, then report its result the way the
    // daemon does (the workspace-member JWT is accepted by the report
    // endpoint). The report warms the server-side catalog cache, which is
    // what the composer's model picker reads and what the send validates
    // against — without it the send is rejected (invalid_model_or_thinking
    // _level) and the composer locks into the runtime-guide state. The
    // runtime still has no daemon, so a sent task stays queued: exactly the
    // window the running-stop group needs.
    const initModelsRes = await authedFetch(
      api,
      `/api/runtimes/${createdRuntimeId}/models`,
      { method: "POST" },
      workspaceSlug,
    );
    expect(initModelsRes.status).toBe(200);
    const initModels = (await initModelsRes.json()) as { id: string };
    const reportModelsRes = await authedFetch(
      api,
      `/api/daemon/runtimes/${createdRuntimeId}/models/${initModels.id}/result`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          status: "completed",
          models: [
            { id: "sonnet", label: "Sonnet", provider: "hermes", default: true },
          ],
          supported: true,
        }),
      },
      workspaceSlug,
    );
    expect(reportModelsRes.status).toBe(200);
  });

  test.afterEach(async () => {
    try {
      if (pgClient) {
        if (createdProjectId) {
          // The send creates a container issue whose project_id FK is
          // ON DELETE SET NULL, so deleting the project would orphan it.
          // Remove issues bound to this project first (their tasks cascade).
          await pgClient.query(`DELETE FROM issue WHERE project_id = $1`, [createdProjectId]);
          await pgClient.query(`DELETE FROM project WHERE id = $1`, [createdProjectId]);
        }
        if (createdAgentId) {
          await pgClient.query(`DELETE FROM agent WHERE id = $1`, [createdAgentId]);
        }
        if (createdRuntimeId) {
          await pgClient.query(`DELETE FROM agent_runtime WHERE id = $1`, [createdRuntimeId]);
        }
      }
    } finally {
      if (pgClient) await pgClient.end();
      pgClient = null;
      createdProjectId = null;
      createdAgentId = null;
      createdRuntimeId = null;
      // Guard for a beforeEach failure (no client, nothing to clean up).
      if (api) await api.cleanup();
    }
  });

  async function openProjectChat(page: Page): Promise<void> {
    const token = api.getToken();
    if (!token) throw new Error("test api client not logged in");
    await page.addInitScript((t) => {
      localStorage.setItem("multica_token", t);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, token);
    await page.goto(`/${workspaceSlug}/projects/${createdProjectId}?tab=chat`, {
      waitUntil: "domcontentloaded",
    });
    await expect(
      projectSurface(page),
      "the project chat composer surface must mount",
    ).toBeVisible({ timeout: 30000 });
  }

  async function sendViaKeyboard(page: Page, text: string): Promise<void> {
    const editor = projectSurface(page).locator(".ProseMirror");
    await editor.click();
    await editor.fill(text);
    await page.keyboard.press("ControlOrMeta+Enter");
  }

  test("(a) wide panel: composer surface edges align with the message column (<1px)", async ({
    page,
  }) => {
    await openProjectChat(page);

    const surface = projectSurface(page);
    const surfaceBox = await surface.boundingBox();
    expect(surfaceBox, "surface bounding box").not.toBeNull();
    // The reading column is the Team Agent stream's inner CHAT_COLUMN layer,
    // directly under the pinned "no earlier messages" divider's parent.
    const column = page.getByTestId("project-chat-no-earlier").locator("..");
    const columnBox = await column.boundingBox();
    expect(columnBox, "message column bounding box").not.toBeNull();

    const leftDrift = Math.abs(surfaceBox!.x - columnBox!.x);
    const rightDrift = Math.abs(
      surfaceBox!.x + surfaceBox!.width - (columnBox!.x + columnBox!.width),
    );
    expect(leftDrift, "left edges must line up within 1px").toBeLessThan(1);
    expect(rightDrift, "right edges must line up within 1px").toBeLessThan(1);

    // Same chrome as the ordinary chat composer (AC-2).
    await expect(surface).toHaveClass(/border-surface-border/);
    await expect(surface).toHaveClass(/bg-surface/);
    await expect(surface).toHaveClass(/rounded-lg/);
  });

  test("(b) 360px narrow panel: no overflow, no overlap", async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 780 });
    await openProjectChat(page);

    // No horizontal overflow anywhere in the project pane.
    const pane = page.locator('[data-testid="project-chat-composer"]').locator("..").locator("..");
    const overflow = await pane.evaluate(
      (el) => el.scrollWidth <= el.clientWidth,
    );
    expect(overflow, "pane must not overflow horizontally").toBe(true);

    // Editor and send button must not overlap: they sit on DIFFERENT rows
    // of the flow bottom bar, so a 1-D x comparison would flag a false
    // positive — non-intersection is the correct overlap model (AC-3).
    const editorBox = await projectSurface(page).locator(".ProseMirror").boundingBox();
    const sendBox = await projectSurface(page)
      .locator('button[aria-label="Send"]')
      .boundingBox();
    expect(editorBox, "editor box").not.toBeNull();
    expect(sendBox, "send button box").not.toBeNull();
    const boxesIntersect = (a: { x: number; y: number; width: number; height: number }, b: { x: number; y: number; width: number; height: number }) =>
      a.x < b.x + b.width && a.x + a.width > b.x && a.y < b.y + b.height && a.y + a.height > b.y;
    expect(
      boxesIntersect(editorBox!, sendBox!),
      "editor and send boxes must not intersect",
    ).toBe(false);

    // Model/thinking chips live in the wrapping left group and stay inside
    // the surface.
    const modelChip = page.getByTestId("project-chat-model-picker");
    if (await modelChip.isVisible()) {
      const chipBox = await modelChip.boundingBox();
      const surfaceBox = await projectSurface(page).boundingBox();
      expect(chipBox!.x, "chip left edge inside the surface").toBeGreaterThanOrEqual(
        surfaceBox!.x,
      );
      expect(
        chipBox!.x + chipBox!.width,
        "chip right edge inside the surface",
      ).toBeLessThanOrEqual(surfaceBox!.x + surfaceBox!.width);
    }
  });

  test("(c) running stop: send queues a task and the bottom-right stop cancels it", async ({
    page,
  }) => {
    await openProjectChat(page);

    await sendViaKeyboard(page, "run this for the stop test");
    // The seeded runtime has no daemon, so the task stays queued: the stop
    // button appears (draft cleared, queue items hold the sent task id).
    const stopButton = projectSurface(page).locator('button[aria-label="Stop"]');
    await expect(stopButton, "stop affordance while the task is active").toBeVisible({
      timeout: 30000,
    });
    await stopButton.click();
    // Cancelling flips the affordance back to send.
    await expect(
      projectSurface(page).locator('button[aria-label="Send"]'),
      "send affordance after the cancel settles",
    ).toBeVisible({ timeout: 30000 });
  });

  test("(d) keyboard send: Mod+Enter sends from the composer", async ({ page }) => {
    await openProjectChat(page);

    const editor = projectSurface(page).locator(".ProseMirror");
    await editor.click();
    await editor.fill("keyboard send smoke");
    await page.keyboard.press("ControlOrMeta+Enter");
    // The keyboard send lands the message in the project's agent queue (the
    // seeded runtime has no daemon, so the task stays queued). The queue bar
    // updates over WS `task:*` — same invalidation the stop affordance
    // relies on. Its count copy is locale-dependent ("1 task queued" vs
    // "1/50 in agent queue"), so the assertion targets the item summary.
    // The stream bubble is deliberately NOT asserted: the container issue
    // only becomes visible to the timeline after the chat context refetches,
    // and the first send does not invalidate it (pre-existing CR-2026-006
    // data flow, outside this CR's scope).
    await page.getByTestId("project-queue-bar-toggle").click();
    await expect(page.getByTestId("project-queue-bar-item")).toContainText(
      "keyboard send smoke",
      { timeout: 30000 },
    );
  });
});
