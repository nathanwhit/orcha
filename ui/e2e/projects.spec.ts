import { test, expect } from "@playwright/test";

// Drives the full add -> edit flow for a registered project through the real UI
// and API, verifying the issue's ask: there is now a way to EDIT an existing
// project, not just add/remove.
test("a project can be added and then edited", async ({ page }) => {
  // Unique repo per run so reruns against a reused server don't collide.
  const repo = `octo/widget-${Date.now()}`;
  // Distinct, non-overlapping names so "old name gone" can be asserted.
  const name = "Widget Alpha";
  const renamed = "Gadget Beta";

  await page.goto("/#/projects");
  await expect(page.getByRole("heading", { name: "Projects" })).toBeVisible();

  // --- Add ---
  await page.getByRole("button", { name: "Add project" }).click();
  await expect(page.getByRole("heading", { name: "Add project" })).toBeVisible();
  await page.getByPlaceholder("defaults to repo").fill(name);
  await page.getByPlaceholder("owner/repo").fill(repo);
  await page.getByPlaceholder("main").fill("develop");
  await page.getByRole("button", { name: "Save project" }).click();

  // The new project shows up in the list with its repo and base branch.
  await expect(page.getByText(repo)).toBeVisible();
  await expect(page.getByText("base develop")).toBeVisible();

  // --- Edit ---
  await page.getByRole("button", { name: "Edit project" }).click();
  await expect(page.getByRole("heading", { name: "Edit project" })).toBeVisible();
  // Fields are pre-filled from the existing project.
  await expect(page.getByPlaceholder("defaults to repo")).toHaveValue(name);
  await expect(page.getByPlaceholder("owner/repo")).toHaveValue(repo);
  await expect(page.getByPlaceholder("main")).toHaveValue("develop");

  // Change the name and base branch, then save.
  await page.getByPlaceholder("defaults to repo").fill(renamed);
  await page.getByPlaceholder("main").fill("trunk");
  await page.getByRole("button", { name: "Save project" }).click();

  // The edit is reflected in the list; the old values are gone.
  await expect(page.getByText(renamed)).toBeVisible();
  await expect(page.getByText("base trunk")).toBeVisible();
  await expect(page.getByText(name)).toHaveCount(0);
  await expect(page.getByText("base develop")).toHaveCount(0);
});
