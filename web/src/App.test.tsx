// SPDX-License-Identifier: AGPL-3.0-only
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import App from "./App";

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve({
        json: () => Promise.resolve({ message: "hello from LastERP" }),
      }),
    ) as unknown as typeof fetch,
  );
});

afterEach(() => vi.unstubAllGlobals());

test("renders the hello message fetched from the kernel API", async () => {
  render(<App />);
  await waitFor(() =>
    expect(screen.getByText("hello from LastERP")).toBeInTheDocument(),
  );
});
