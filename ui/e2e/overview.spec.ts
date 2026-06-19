import { test, expect } from "@playwright/test";

// Verifies that the Overview page keeps only ongoing objectives (active /
// waiting_user) in the main table and tucks succeeded/failed objectives into a
// collapsed-by-default "Completed objectives" section, while canceled ones live
// under their own "Canceled objectives" section.
//
// There is no HTTP endpoint to push an objective into succeeded/failed, so we
// intercept the polled endpoints and return fixture JSON covering every status.
test("completed and canceled objectives are separated from the active table", async ({
  page,
}) => {
  const objectives = [
    {
      id: "obj-active",
      status: "active",
      title: "Active-Objective-AAA",
      repo: "octo/repo",
      active_sessions: 2,
      pr_count: 1,
      open_questions: 0,
      needs_user: false,
      latest_activity: "working",
      updated_at: "2026-06-19T10:00:00Z",
    },
    {
      id: "obj-waiting",
      status: "waiting_user",
      title: "Waiting-Objective-BBB",
      repo: "octo/repo",
      active_sessions: 1,
      pr_count: 0,
      open_questions: 1,
      needs_user: true,
      latest_activity: "needs input",
      updated_at: "2026-06-19T09:00:00Z",
    },
    {
      id: "obj-succeeded",
      status: "succeeded",
      title: "Succeeded-Objective-CCC",
      repo: "octo/repo",
      active_sessions: 0,
      pr_count: 3,
      open_questions: 0,
      needs_user: false,
      latest_activity: "done",
      updated_at: "2026-06-18T08:00:00Z",
    },
    {
      id: "obj-failed",
      status: "failed",
      title: "Failed-Objective-DDD",
      repo: "octo/repo",
      active_sessions: 0,
      pr_count: 0,
      open_questions: 0,
      needs_user: false,
      latest_activity: "errored",
      updated_at: "2026-06-17T07:00:00Z",
    },
    {
      id: "obj-canceled",
      status: "canceled",
      title: "Canceled-Objective-EEE",
      repo: "octo/repo",
      active_sessions: 0,
      pr_count: 0,
      open_questions: 0,
      needs_user: false,
      latest_activity: "stopped",
      updated_at: "2026-06-16T06:00:00Z",
    },
  ];

  // Routes must be registered before the first navigation so the initial poll
  // is intercepted.
  await page.route("**/api/objectives", (route) =>
    route.fulfill({ json: objectives }),
  );
  await page.route("**/api/sessions", (route) => route.fulfill({ json: [] }));
  await page.route("**/api/pull-requests", (route) =>
    route.fulfill({ json: [] }),
  );
  await page.route("**/api/questions", (route) => route.fulfill({ json: [] }));

  await page.goto("/#/");
  await expect(
    page.getByRole("heading", { name: "Overview" }),
  ).toBeVisible();

  // Ongoing objectives show in the main table.
  await expect(page.getByText("Active-Objective-AAA")).toBeVisible();
  await expect(page.getByText("Waiting-Objective-BBB")).toBeVisible();

  // Completed / canceled objectives are hidden behind collapsed sections.
  await expect(page.getByText("Succeeded-Objective-CCC")).toHaveCount(0);
  await expect(page.getByText("Failed-Objective-DDD")).toHaveCount(0);
  await expect(page.getByText("Canceled-Objective-EEE")).toHaveCount(0);

  // Expanding "Completed objectives" reveals the succeeded/failed titles.
  await page
    .getByRole("button", { name: /Completed objectives/ })
    .click();
  await expect(page.getByText("Succeeded-Objective-CCC")).toBeVisible();
  await expect(page.getByText("Failed-Objective-DDD")).toBeVisible();
  // The canceled objective is still hidden under its own section.
  await expect(page.getByText("Canceled-Objective-EEE")).toHaveCount(0);

  // Expanding "Canceled objectives" reveals the canceled title.
  await page
    .getByRole("button", { name: /Canceled objectives/ })
    .click();
  await expect(page.getByText("Canceled-Objective-EEE")).toBeVisible();
});
