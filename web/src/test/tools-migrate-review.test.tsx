import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { components } from "@/lib/api/client";
import { ToolsMigratePage } from "@/routes/tools-migrate";
import { migrationPending, migrationPendingTemplates, type Migration } from "@/test/migration-pending";

type Review = components["schemas"]["MigrationReview"];
type Disk = components["schemas"]["MigrationDisk"];
type Boot = components["schemas"]["MigrationBoot"];

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: vi.fn(),
  },
}));

const ok = (data: unknown): { data: unknown; response: { ok: boolean } } => ({ data, response: { ok: true } });

function reviewOf(migration: Migration): Review {
  const review = migration.report?.review;
  if (!review) {
    throw new Error("the migration-pending fixture has a review");
  }
  return review;
}

function withReview(patch: (review: Review) => Review): Migration {
  const report = migrationPending.report;
  if (!report) {
    throw new Error("the migration-pending fixture has a report");
  }
  return { ...migrationPending, report: { ...report, review: patch(reviewOf(migrationPending)) } };
}

function withBoot(boot: Boot): Migration {
  return withReview((review) => ({ ...review, boot }));
}

function withDisks(patch: (disks: Disk[]) => Disk[]): Migration {
  return withReview((review) => ({ ...review, disks: patch(review.disks) }));
}

function withUnknownHostBoot(migration: Migration): Migration {
  const report = migration.report;
  if (!report?.review) {
    throw new Error("the fixture has a report with a review");
  }
  const disks = report.review.disks.map((disk) => ({ ...disk, hostBoot: undefined }));
  return { ...migration, report: { ...report, review: { ...report.review, disks } } };
}

function withHostBoot(slot: string, hostBoot: boolean, extra: Partial<Disk> = {}): (disks: Disk[]) => Disk[] {
  return (disks) => disks.map((disk) => (disk.slot === slot ? { ...disk, ...extra, hostBoot } : disk));
}

function internalSharedWith(hostBoot: boolean): Migration {
  return withReview((review) => ({
    ...review,
    boot: { mode: "internal", mirrored: false, sharedWithCache: true },
    disks: [...review.disks.filter((disk) => disk.slot !== "pool cache"), { ...SHARED_CACHE_ROW, hostBoot }],
  }));
}

function renderPage(migration: Migration): ReturnType<typeof render> {
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/migrate":
        return Promise.resolve(ok(migration));
      case "/migrate/templates":
        return Promise.resolve(ok(migrationPendingTemplates));
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
  return render(
    <MemoryRouter>
      <ToolsMigratePage />
    </MemoryRouter>,
  );
}

async function selectRole(disk: string, roleName: string): Promise<void> {
  fireEvent.click(await screen.findByRole("combobox", { name: `Role for ${disk}` }));
  const option = await screen.findByRole("option", { name: roleName });
  fireEvent.pointerDown(option, { pointerType: "mouse" });
  fireEvent.pointerUp(option, { pointerType: "mouse" });
  fireEvent.click(option);
}

function roleOf(disk: string): string {
  return screen.getByRole("combobox", { name: `Role for ${disk}` }).textContent ?? "";
}

function rowOf(text: string): HTMLElement {
  const row = screen.getByText(text).closest("tr");
  if (!row) {
    throw new Error(`${text} is not in a table row`);
  }
  return row;
}

const SHARED_CACHE_ROW: Disk = {
  slot: "pool cache",
  unraidRole: "cache",
  proposedRole: "cache",
  unraidBoot: true,
  device: "/dev/nvme1n1",
  serial: "EXAMPLE_BOOTNVME",
  size: 1000204886016,
  filesystem: "zfs_member",
  weakIdentity: false,
  refused: false,
};

describe("the migration Review step", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
  });

  describe("the disk mapping", () => {
    it("is a table of the structured disks, with serial, Unraid disk number, size, filesystem and a role each", async () => {
      renderPage(migrationPending);

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_DISK1" });
      for (const header of ["Disk (serial number)", "Unraid disk number", "Size", "Filesystem", "Role in Hoserva"]) {
        expect(screen.getByRole("columnheader", { name: header })).toBeInTheDocument();
      }
      const row = within(rowOf("EXAMPLE_DISK1"));
      expect(row.getByText("1")).toBeInTheDocument();
      expect(row.getByText("4.00 TiB")).toBeInTheDocument();
      expect(row.getByText("xfs")).toBeInTheDocument();
      expect(screen.getAllByRole("combobox", { name: /^Role for / })).toHaveLength(reviewOf(migrationPending).disks.length);
    });

    it("is pre-filled with the proposed roles and leaves a disk with none proposed not chosen", async () => {
      renderPage(migrationPending);

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_DISK1" });
      expect(roleOf("EXAMPLE_PARITY")).toBe("Parity");
      expect(roleOf("EXAMPLE_DISK1")).toBe("Data");
      expect(roleOf("EXAMPLE_CACHE")).toBe("Cache");
      expect(roleOf("4C530001240603119335")).toBe("Ignore");
      expect(roleOf("EXAMPLE_DISK2")).toBe("Not chosen");
      expect(roleOf("EXAMPLE_DISK3")).toBe("Not chosen");
      expect(roleOf("EXAMPLE_SPARE")).toBe("Not chosen");
    });

    it("shows what the API leaves out as unknown, not as a value", async () => {
      renderPage(migrationPending);

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_DISK2" });
      const row = within(rowOf("EXAMPLE_DISK2"));
      expect(row.getByText("Unknown")).toBeInTheDocument();
      expect(row.getByText(/no disk on this machine has this serial or WWN/)).toBeInTheDocument();
      expect(row.queryByText("Weak identity")).not.toBeInTheDocument();
    });

    it("shows the refusal and rejects a refused disk as a data disk, and only then", async () => {
      renderPage(migrationPending);

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_DISK3" });
      expect(within(rowOf("EXAMPLE_DISK3")).getByText("Not adopted")).toBeInTheDocument();
      expect(within(rowOf("EXAMPLE_DISK3")).getByText(/its read-only xfs check failed/)).toBeInTheDocument();
      expect(screen.queryByText(/so it cannot be a data disk/)).not.toBeInTheDocument();

      await selectRole("EXAMPLE_DISK3", "Data");
      expect(await screen.findByText("The scan did not adopt this disk, so it cannot be a data disk.")).toBeInTheDocument();

      await selectRole("EXAMPLE_DISK3", "Ignore");
      expect(screen.queryByText(/so it cannot be a data disk/)).not.toBeInTheDocument();
    });

    it("rejects a weak-identity disk as a parity disk", async () => {
      renderPage(migrationPending);

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_DISK4" });
      expect(within(rowOf("EXAMPLE_DISK4")).getByText("Weak identity")).toBeInTheDocument();
      expect(screen.queryByText(/has no WWN or serial number to tell it apart/)).not.toBeInTheDocument();

      await selectRole("EXAMPLE_DISK4", "Parity");
      expect(
        await screen.findByText("This disk has no WWN or serial number to tell it apart, so it cannot be a parity disk."),
      ).toBeInTheDocument();
    });

    it("rejects a disk Unraid used for parity as a data disk", async () => {
      renderPage(migrationPending);

      await selectRole("EXAMPLE_PARITY", "Data");

      expect(await screen.findByText("Unraid used this disk for parity, so it cannot become a data disk.")).toBeInTheDocument();
    });

    it("rejects the disk this machine boots from as a parity disk", async () => {
      renderPage(
        withDisks((disks) =>
          disks.map((disk) =>
            disk.slot === "parity" ? { ...disk, refused: true, refusalCode: "host_boot", refusal: "it can never be a parity disk." } : disk,
          ),
        ),
      );

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_PARITY" });
      await selectRole("EXAMPLE_PARITY", "Ignore");
      expect(screen.queryByText("This is the disk this machine boots from, so it can never be a parity disk.")).not.toBeInTheDocument();

      await selectRole("EXAMPLE_PARITY", "Parity");
      expect(await screen.findByText("This is the disk this machine boots from, so it can never be a parity disk.")).toBeInTheDocument();
    });

    it("allows an Unraid boot device only to be ignored", async () => {
      renderPage(migrationPending);

      await screen.findByRole("combobox", { name: "Role for 4C530001240603119335" });
      expect(within(rowOf("4C530001240603119335")).getByText("Unraid boot device")).toBeInTheDocument();
      expect(screen.queryByText("An Unraid boot device can only be ignored.")).not.toBeInTheDocument();

      await selectRole("4C530001240603119335", "Data");
      expect(await screen.findByText("An Unraid boot device can only be ignored.")).toBeInTheDocument();
    });

    it("lets the cache row of an internal boot that shares its disk with the cache stay cache, and no other role but ignore", async () => {
      renderPage(withDisks((disks) => [...disks.filter((disk) => disk.slot !== "pool cache"), SHARED_CACHE_ROW]));

      await screen.findByRole("combobox", { name: "Role for EXAMPLE_BOOTNVME" });
      expect(roleOf("EXAMPLE_BOOTNVME")).toBe("Cache");
      expect(screen.queryByText("An Unraid boot device can only be ignored.")).not.toBeInTheDocument();

      await selectRole("EXAMPLE_BOOTNVME", "Data");
      expect(await screen.findByText("An Unraid boot device can only be ignored.")).toBeInTheDocument();

      await selectRole("EXAMPLE_BOOTNVME", "Ignore");
      expect(screen.queryByText("An Unraid boot device can only be ignored.")).not.toBeInTheDocument();
    });

    it("asks for one or two parity disks", async () => {
      renderPage(migrationPending);
      await screen.findByRole("combobox", { name: "Role for EXAMPLE_PARITY" });
      expect(screen.queryByText("No parity disk is chosen")).not.toBeInTheDocument();
      expect(screen.queryByText("Too many parity disks are chosen")).not.toBeInTheDocument();

      await selectRole("EXAMPLE_PARITY", "Ignore");
      expect(await screen.findByText("No parity disk is chosen")).toBeInTheDocument();

      await selectRole("EXAMPLE_PARITY", "Parity");
      await selectRole("EXAMPLE_DISK1", "Parity");
      expect(screen.queryByText("No parity disk is chosen")).not.toBeInTheDocument();
      expect(screen.queryByText("Too many parity disks are chosen")).not.toBeInTheDocument();

      await selectRole("EXAMPLE_SPARE", "Parity");
      expect(await screen.findByText("Too many parity disks are chosen")).toBeInTheDocument();
    });

    it("keeps the edited roles when the user goes back to the scan and returns to Review", async () => {
      renderPage(migrationPending);
      await selectRole("EXAMPLE_SPARE", "Cache");
      expect(roleOf("EXAMPLE_SPARE")).toBe("Cache");

      fireEvent.click(screen.getByRole("button", { name: "Back" }));
      await screen.findByText("Step 1 of 4");
      fireEvent.click(screen.getByRole("button", { name: "Review" }));

      expect(await screen.findByText("Step 2 of 4")).toBeInTheDocument();
      expect(roleOf("EXAMPLE_SPARE")).toBe("Cache");
      expect(roleOf("EXAMPLE_DISK1")).toBe("Data");
    });
  });

  describe("the share preview", () => {
    it("is a table of the shares with a warning count each", async () => {
      renderPage(migrationPending);

      await screen.findByText("media");
      expect(within(rowOf("media")).getByText("1 warning")).toBeInTheDocument();
      expect(within(rowOf("backup")).getByText("No warnings")).toBeInTheDocument();
      expect(within(rowOf("backup")).getByText("Fill-up")).toBeInTheDocument();
      expect(within(rowOf("backup")).getByText("Not disk3")).toBeInTheDocument();
      expect(within(rowOf("documents")).getByText("Most-free")).toBeInTheDocument();
      expect(within(rowOf("documents")).getByText("Any disk")).toBeInTheDocument();
    });

    it("carries the High-water note, with the policy it becomes, for a High-water share", async () => {
      renderPage(migrationPending);

      await screen.findByText("media");
      const row = within(rowOf("media"));
      expect(row.getByText("High-water")).toBeInTheDocument();
      expect(row.getByText("(mfs)")).toBeInTheDocument();
      expect(screen.getByText(/High-water has no exact equivalent in Hoserva/)).toBeInTheDocument();
    });

    it("has no High-water note when no share uses High-water", async () => {
      renderPage(
        withReview((review) => ({
          ...review,
          shares: review.shares.map((share) => ({ ...share, highWater: false, allocationMethod: "fillup" })),
        })),
      );

      await screen.findByText("media");
      expect(screen.queryByText(/High-water has no exact equivalent/)).not.toBeInTheDocument();
    });

    it("says so when the configuration has no shares, not an empty table", async () => {
      renderPage(withReview((review) => ({ ...review, shares: [] })));

      expect(await screen.findByText("The Unraid configuration has no shares.")).toBeInTheDocument();
    });
  });

  describe("the boot mode and planned layout", () => {
    it("shows a USB boot, the cache the roles give, and both rollback rows when no disk says whether it is the host boot disk", async () => {
      renderPage(withUnknownHostBoot(withBoot({ mode: "usb" })));

      await screen.findByText("Boot mode and layout");
      expect(screen.getByText("A USB stick")).toBeInTheDocument();
      expect(screen.getByText("On EXAMPLE_CACHE, from the roles above")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in and boot from it.")).toBeInTheDocument();
      expect(
        screen.getByText("Put the stick back in, re-create the Unraid cache and move the appdata back."),
      ).toBeInTheDocument();
    });

    it("shows only the separate-device row for a USB boot whose cache is not on the disk this machine boots from", async () => {
      renderPage(withBoot({ mode: "usb" }));

      expect(await screen.findByText("A separate boot device")).toBeInTheDocument();
      expect(screen.getByText("Where Debian goes")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in and boot from it.")).toBeInTheDocument();
      expect(screen.queryByText("Put the stick back in, re-create the Unraid cache and move the appdata back.")).not.toBeInTheDocument();
      expect(screen.queryByText(/the Unraid cache is destroyed by the installation/)).not.toBeInTheDocument();
    });

    it("shows only the shared NVMe row for a USB boot whose cache is on the disk this machine boots from", async () => {
      renderPage(withDisks(withHostBoot("pool cache", true)));
      await screen.findByText("Boot mode and layout");

      expect(await screen.findByText("A shared NVMe: the cache is on the disk this machine boots from")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in, re-create the Unraid cache and move the appdata back.")).toBeInTheDocument();
      expect(screen.queryByText("Put the stick back in and boot from it.")).not.toBeInTheDocument();
      expect(screen.getByText(/the Unraid cache is destroyed by the installation/)).toBeInTheDocument();
    });

    it("keeps the shared NVMe row when Unraid's cache disk is set to Ignore and the cache goes to another disk", async () => {
      renderPage(withDisks(withHostBoot("pool cache", true)));
      expect(await screen.findByText("A shared NVMe: the cache is on the disk this machine boots from")).toBeInTheDocument();

      await selectRole("EXAMPLE_SPARE", "Cache");
      expect(await screen.findByText("On EXAMPLE_CACHE, EXAMPLE_SPARE, from the roles above")).toBeInTheDocument();
      expect(screen.getByText("A shared NVMe: the cache is on the disk this machine boots from")).toBeInTheDocument();

      await selectRole("EXAMPLE_CACHE", "Ignore");
      expect(await screen.findByText("On EXAMPLE_SPARE, from the roles above")).toBeInTheDocument();
      expect(screen.getByText("A shared NVMe: the cache is on the disk this machine boots from")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in, re-create the Unraid cache and move the appdata back.")).toBeInTheDocument();
      expect(screen.queryByText("Put the stick back in and boot from it.")).not.toBeInTheDocument();
      expect(screen.queryByText("A separate boot device")).not.toBeInTheDocument();
      expect(screen.getByText(/the Unraid cache is destroyed by the installation/)).toBeInTheDocument();
    });

    it("keeps the separate-device row when the cache role moves to another disk and Unraid's cache is not the boot disk", async () => {
      renderPage(withBoot({ mode: "usb" }));
      await screen.findByText("A separate boot device");

      await selectRole("EXAMPLE_SPARE", "Cache");
      await selectRole("EXAMPLE_CACHE", "Ignore");
      expect(await screen.findByText("On EXAMPLE_SPARE, from the roles above")).toBeInTheDocument();
      expect(screen.getByText("A separate boot device")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in and boot from it.")).toBeInTheDocument();
    });

    it("decides nothing when Unraid's cache disk has no hostBoot, whatever disk the cache role is on", async () => {
      renderPage(withDisks((disks) => disks.map((disk) => ({ ...disk, hostBoot: disk.slot === "pool cache" ? undefined : false }))));
      await screen.findByText("On EXAMPLE_CACHE, from the roles above");

      await selectRole("EXAMPLE_SPARE", "Cache");
      await selectRole("EXAMPLE_CACHE", "Ignore");
      expect(await screen.findByText("On EXAMPLE_SPARE, from the roles above")).toBeInTheDocument();
      expect(screen.queryByText("Where Debian goes")).not.toBeInTheDocument();
      expect(screen.queryByText("A separate boot device")).not.toBeInTheDocument();
      expect(screen.getByText("Put the stick back in and boot from it.")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in, re-create the Unraid cache and move the appdata back.")).toBeInTheDocument();
    });

    it("shows every row for the boot kind when no cache is chosen", async () => {
      renderPage(withBoot({ mode: "usb" }));
      await screen.findByText("A separate boot device");

      await selectRole("EXAMPLE_CACHE", "Ignore");
      expect(await screen.findByText("No disk has the cache role")).toBeInTheDocument();
      expect(screen.queryByText("Where Debian goes")).not.toBeInTheDocument();
      expect(screen.getByText("Put the stick back in and boot from it.")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in, re-create the Unraid cache and move the appdata back.")).toBeInTheDocument();
    });

    it("does not read an absent hostBoot as a separate device", async () => {
      renderPage(withDisks((disks) => disks.map((disk) => ({ ...disk, hostBoot: disk.slot === "pool cache" ? undefined : false }))));

      expect(await screen.findByText("On EXAMPLE_CACHE, from the roles above")).toBeInTheDocument();
      expect(screen.queryByText("Where Debian goes")).not.toBeInTheDocument();
      expect(screen.getByText("Put the stick back in and boot from it.")).toBeInTheDocument();
      expect(screen.getByText("Put the stick back in, re-create the Unraid cache and move the appdata back.")).toBeInTheDocument();
    });

    it("shows only the same-NVMe row for an internal boot holding the cache when Debian is on that disk", async () => {
      renderPage(internalSharedWith(true));

      expect(await screen.findByText("A shared NVMe: the cache is on the disk this machine boots from")).toBeInTheDocument();
      expect(screen.getByText("The same NVMe")).toBeInTheDocument();
      expect(screen.getByText("Restore the Flash Backup zip to a USB stick and boot from it, then re-create the cache.")).toBeInTheDocument();
      expect(screen.queryByText("Another device")).not.toBeInTheDocument();
    });

    it("shows only the another-device row for an internal boot holding the cache when Debian is on another disk", async () => {
      renderPage(internalSharedWith(false));

      expect(await screen.findByText("A separate boot device")).toBeInTheDocument();
      expect(screen.getByText("Another device")).toBeInTheDocument();
      expect(screen.getByText(/The cache, partition 4, is untouched until parity is first written/)).toBeInTheDocument();
      expect(screen.queryByText("The same NVMe")).not.toBeInTheDocument();
    });

    it("follows the cache role chosen in the mapping", async () => {
      renderPage(withBoot({ mode: "usb" }));
      await screen.findByText("On EXAMPLE_CACHE, from the roles above");

      await selectRole("EXAMPLE_CACHE", "Ignore");
      expect(await screen.findByText("No disk has the cache role")).toBeInTheDocument();

      await selectRole("EXAMPLE_SPARE", "Cache");
      expect(await screen.findByText("On EXAMPLE_SPARE, from the roles above")).toBeInTheDocument();
    });

    it("shows an internal device of its own with its two rollback rows", async () => {
      renderPage(withBoot({ mode: "internal", mirrored: false, sharedWithCache: false }));

      await screen.findByText("An internal device of its own");
      expect(screen.getByText("Switch the firmware boot order back.")).toBeInTheDocument();
      expect(screen.getByText(/Restore the Flash Backup zip to a USB stick with the Unraid USB Flash Creator/)).toBeInTheDocument();
      expect(screen.queryByText("Put the stick back in and boot from it.")).not.toBeInTheDocument();
    });

    it("shows a mirrored pair with its three rollback rows", async () => {
      renderPage(withBoot({ mode: "internal", mirrored: true, sharedWithCache: false }));

      await screen.findByText("A mirrored pair of internal devices");
      expect(screen.getByText("One device of the pair")).toBeInTheDocument();
      expect(screen.getByText(/Unraid runs with a degraded boot pool/)).toBeInTheDocument();
      expect(screen.getByText("Both devices")).toBeInTheDocument();
    });

    it("shows an internal boot that also holds the cache with its two rollback rows", async () => {
      renderPage(withBoot({ mode: "internal", mirrored: false, sharedWithCache: true }));

      await screen.findByText("An internal device that also holds the Unraid cache");
      expect(screen.getByText("The same NVMe")).toBeInTheDocument();
      expect(screen.getByText("Restore the Flash Backup zip to a USB stick and boot from it, then re-create the cache.")).toBeInTheDocument();
      expect(screen.getByText(/The cache, partition 4, is untouched until parity is first written/)).toBeInTheDocument();
    });

    it("promises no rollback for an internal boot the capture does not describe fully", async () => {
      renderPage(withBoot({ mode: "internal" }));

      await screen.findByText("An internal device");
      expect(screen.getByText(/does not say whether the internal boot device is mirrored/)).toBeInTheDocument();
      expect(screen.queryByText("Going back to Unraid", { selector: "th" })).not.toBeInTheDocument();
    });

    it("promises no rollback for a mirrored internal boot that also holds the cache, and says the table has no row for it", async () => {
      renderPage(withBoot({ mode: "internal", mirrored: true, sharedWithCache: true }));

      await screen.findByText("A mirrored pair of internal devices that also holds the Unraid cache");
      expect(screen.getByText(/rollback table has no row for that combination/)).toBeInTheDocument();
      expect(screen.queryByText(/does not say whether the internal boot device is mirrored/)).not.toBeInTheDocument();
      expect(screen.queryByText("Going back to Unraid", { selector: "th" })).not.toBeInTheDocument();
    });

    it("shows an unknown boot mode as unknown, not as a USB stick", async () => {
      renderPage(withBoot({}));

      await screen.findByText("Boot mode and layout");
      expect(screen.getByText("Unknown", { selector: "dd" })).toBeInTheDocument();
      expect(screen.queryByText("A USB stick")).not.toBeInTheDocument();
      expect(screen.getByText(/does not say where Unraid boots from/)).toBeInTheDocument();
      expect(screen.queryByText("Put the stick back in and boot from it.")).not.toBeInTheDocument();
    });
  });
});
