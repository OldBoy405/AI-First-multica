// @vitest-environment jsdom

/**
 * The status filter's category headings, against the REAL Base UI menu
 * primitives (MUL-6393).
 *
 * `DropdownMenuLabel` renders Base UI's `Menu.GroupLabel`, whose
 * `useMenuGroupRootContext()` THROWS when it has no `Menu.Group` ancestor.
 * The heading only renders once a workspace holds a custom status, so the
 * missing group stayed invisible until the first one was created — and then
 * opening the filter menu took the whole app down, because no error boundary
 * sits above the issues surface. Same failure as MUL-4819, one menu over.
 *
 * These tests therefore must NOT mock `@multica/ui/components/ui/dropdown-menu`:
 * a flattened mock renders a heading outside a group perfectly happily, which
 * is exactly how the bug shipped.
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createStore } from "zustand/vanilla";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { STATUS_ORDER } from "@multica/core/issues/config";
import {
  type IssueViewState,
  viewStoreSlice,
} from "@multica/core/issues/stores/view-store";
import { ViewStoreProvider } from "@multica/core/issues/stores/view-store-context";
import type { IssueStatusEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { IssueFilterMenu } from "./issues-header";

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

function entry(overrides: Partial<IssueStatusEntry>): IssueStatusEntry {
  return {
    id: `s-${overrides.key}`,
    workspace_id: "ws-1",
    key: "todo",
    name: "Todo",
    description: "",
    category: "todo",
    color: "#888888",
    is_system: true,
    position: 0,
    archived_at: null,
    created_at: "",
    updated_at: "",
    ...overrides,
  } as IssueStatusEntry;
}

/** What the server seeds every workspace with: one built-in per category. */
const BUILT_INS = STATUS_ORDER.map((category) =>
  entry({ key: category, name: category, category, is_system: true }),
);

const HUMAN_REVIEW = entry({
  key: "human_review",
  name: "Human Review",
  category: "in_review",
  color: "#8b5cf6",
  is_system: false,
  position: 1,
});

function renderFilterMenu(statuses: IssueStatusEntry[]) {
  setApiInstance({
    listIssueStatuses: async () => ({
      statuses,
      categories: [],
      total: statuses.length,
    }),
    listProperties: async () => ({ properties: [] }),
  } as unknown as ApiClient);

  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  const store = createStore<IssueViewState>()(viewStoreSlice);

  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <ViewStoreProvider store={store}>
        <IssueFilterMenu trigger={<button type="button">Filter</button>} />
      </ViewStoreProvider>
    </QueryClientProvider>,
  );
}

/** Opens the Filter menu, then its Status submenu. */
async function openStatusSubmenu() {
  fireEvent.click(screen.getByRole("button", { name: "Filter" }));
  const statusTrigger = await screen.findByRole("menuitem", { name: /^Status/ });
  fireEvent.click(statusTrigger);
  await waitFor(() =>
    expect(screen.getByRole("menuitemcheckbox", { name: /In Review/ })).toBeInTheDocument(),
  );
}

afterEach(() => {
  cleanup();
  // Base UI portals the menu onto document.body; leftovers would duplicate
  // labels across tests.
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

describe("IssueFilterMenu status section", () => {
  it("opens with a custom status in the catalog instead of crashing on the category heading", async () => {
    renderFilterMenu([...BUILT_INS, HUMAN_REVIEW]);

    // Reaching this at all is the regression: the heading below used to throw
    // out of Base UI's Menu.GroupLabel and unmount the app.
    await openStatusSubmenu();

    expect(
      screen.getByRole("menuitemcheckbox", { name: /Human Review/ }),
    ).toBeInTheDocument();
    // The heading is present AND owns a group — Base UI wires it up as the
    // group's accessible name, which is only possible inside Menu.Group.
    const heading = screen.getByText("In Review", {
      selector: "[data-slot='dropdown-menu-label']",
    });
    const group = heading.closest("[data-slot='dropdown-menu-group']");
    expect(group).not.toBeNull();
    expect(group?.getAttribute("aria-labelledby")).toBe(heading.id);
    // Both In Review statuses sit under that one heading.
    expect(group?.querySelectorAll("[role='menuitemcheckbox']")).toHaveLength(2);
  });

  it("keeps the flat, heading-free list for a workspace with no custom statuses", async () => {
    renderFilterMenu(BUILT_INS);

    await openStatusSubmenu();

    expect(
      document.querySelector("[data-slot='dropdown-menu-label']"),
    ).toBeNull();
    expect(screen.getAllByRole("menuitemcheckbox")).toHaveLength(STATUS_ORDER.length);
  });
});
