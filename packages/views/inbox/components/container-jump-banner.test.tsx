// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import { NavigationProvider } from "../../navigation/context";
import type { NavigationAdapter } from "../../navigation/types";
import enCommon from "../../locales/en/common.json";
import enInbox from "../../locales/en/inbox.json";

const TEST_RESOURCES = { en: { common: enCommon, inbox: enInbox } };

function makeAdapter(overrides: Partial<NavigationAdapter> = {}): NavigationAdapter {
  return {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/",
    searchParams: new URLSearchParams(),
    getShareableUrl: (p) => p,
    ...overrides,
  };
}

import { ContainerJumpBanner } from "./container-jump-banner";

function renderBanner(mode: "team_agent" | "discussion", adapter = makeAdapter()) {
  return render(
    <NavigationProvider value={adapter}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ContainerJumpBanner projectHref="/ws/projects/proj-1?tab=chat&mode=discussion" mode={mode} />
      </I18nProvider>
    </NavigationProvider>,
  );
}

describe("ContainerJumpBanner (CR-2026-009 TASK-04)", () => {
  afterEach(cleanup);

  it("shows the Discussion-specific copy and navigates to the given href on click", () => {
    const push = vi.fn();
    renderBanner("discussion", makeAdapter({ push }));
    expect(screen.getByText(/part of the project's Discussion/)).toBeTruthy();
    fireEvent.click(screen.getByText("Go to project chat"));
    expect(push).toHaveBeenCalledWith("/ws/projects/proj-1?tab=chat&mode=discussion");
  });

  it("shows the Team Agent-specific copy for the team_agent mode", () => {
    renderBanner("team_agent");
    expect(screen.getByText(/part of the project's Team Agent chat/)).toBeTruthy();
  });
});
