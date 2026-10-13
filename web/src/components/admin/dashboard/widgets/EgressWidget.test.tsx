import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { AdminTimeseries } from "@/api/types";

const mocks = vi.hoisted(() => ({
  useAdminTimeseries: vi.fn(),
}));

vi.mock("@/hooks/queries/admin/dashboardInsights", () => ({
  useAdminTimeseries: mocks.useAdminTimeseries,
}));

import { EgressWidget } from "./EgressWidget";

const HOUR_MS = 3_600_000;

function series(overrides: Partial<AdminTimeseries> = {}): AdminTimeseries {
  const to = Date.now();
  const from = to - 24 * HOUR_MS;
  return {
    resolution_seconds: 300,
    from: new Date(from).toISOString(),
    to: new Date(to).toISOString(),
    oldest_sample_at: new Date(from - 7 * 24 * HOUR_MS).toISOString(),
    points: [
      {
        t: new Date(to - HOUR_MS).toISOString(),
        streams: 1,
        direct: 1,
        remux: 0,
        transcode: 0,
        egress_kbps: 56_000,
      },
    ],
    ...overrides,
  };
}

describe("EgressWidget", () => {
  beforeEach(() => {
    mocks.useAdminTimeseries.mockReset();
  });

  it("shows the server's transfer total beside the peak", () => {
    mocks.useAdminTimeseries.mockReturnValue({ data: series({ egress_bytes: 1_400_000_000_000 }) });

    render(<EgressWidget />);

    const total = screen.getByText("Total 1.4 TB");
    expect(total.getAttribute("title")).toMatch(/^About 1\.4 TB served over /);
    expect(screen.getByText("Peak 56 Mbps")).toBeTruthy();
  });

  it("breaks out download traffic in the tooltip when there was some", () => {
    mocks.useAdminTimeseries.mockReturnValue({
      data: series({ egress_bytes: 1_400_000_000_000, download_egress_bytes: 37_800_000_000 }),
    });

    render(<EgressWidget />);

    expect(screen.getByText("Total 1.4 TB").getAttribute("title")).toMatch(
      /^About 1\.4 TB \(37\.8 GB of it downloads\) served over /,
    );
  });

  it("shows a zero total, which is a real answer", () => {
    mocks.useAdminTimeseries.mockReturnValue({ data: series({ egress_bytes: 0 }) });

    render(<EgressWidget />);

    expect(screen.getByText("Total 0 B")).toBeTruthy();
  });

  it("hides the total when the server does not send one", () => {
    mocks.useAdminTimeseries.mockReturnValue({ data: series() });

    render(<EgressWidget />);

    expect(screen.queryByText(/^Total /)).toBeNull();
    expect(screen.getByText("Peak 56 Mbps")).toBeTruthy();
  });

  it("says when sampling began inside the window", () => {
    const data = series({ egress_bytes: 2_000_000 });
    data.oldest_sample_at = new Date(Date.parse(data.from) + 6 * HOUR_MS).toISOString();
    mocks.useAdminTimeseries.mockReturnValue({ data });

    render(<EgressWidget />);

    expect(screen.getByText("Total 2.0 MB").getAttribute("title")).toMatch(
      /^About 2\.0 MB served since .+, when sampling began\./,
    );
  });
});
