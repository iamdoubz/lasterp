// SPDX-License-Identifier: AGPL-3.0-only

// Route tree. Built with the code-based API rather than file-based routing: the
// object screens are one parameterised route each, not one file per object —
// which objects exist is a runtime question answered by /api/v1/meta/objects.

import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  useParams,
} from "@tanstack/react-router";

import { getRecord } from "./api";
import { useT } from "./i18n";
import { DashboardScreen } from "./routes/DashboardView";
import { InvoiceDetail } from "./routes/InvoiceDetail";
import { ObjectDetail } from "./routes/ObjectDetail";
import { ObjectForm } from "./routes/ObjectForm";
import { ObjectList } from "./routes/ObjectList";
import { Shell, useObjects } from "./routes/Shell";
import { Alert, Busy } from "./ui";
import { useQuery } from "@tanstack/react-query";

export interface RouterContext {
  onSignedOut: () => void;
}

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: function Root() {
    const { onSignedOut } = rootRoute.useRouteContext();
    return <Shell onSignedOut={onSignedOut} />;
  },
});

/** useObject resolves the :resource path param to its schema. Rendering waits
 * on the metadata — without it there is nothing to render from. */
function useObject(resource: string) {
  const { data, isPending, error } = useObjects();
  const object = data?.find((o) => o.resource === resource);
  return { object, isPending, error };
}

/** ObjectGate handles the three states every object screen shares, so no screen
 * repeats them: metadata loading, metadata failed, unknown resource. */
function ObjectGate({
  resource,
  children,
}: {
  resource: string;
  children: (object: NonNullable<ReturnType<typeof useObject>["object"]>) => React.ReactNode;
}) {
  const t = useT();
  const { object, isPending, error } = useObject(resource);

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("status.error")}</Alert>;
  }
  if (!object) {
    // Either the object does not exist or its module is disabled for this
    // tenant — indistinguishable to the client, and both mean "no screen".
    return <Alert>{t("status.capabilityDisabled")}</Alert>;
  }
  return <>{children(object)}</>;
}

// Landing on the role dashboard is docs/21 §4's "new user logs in → their
// role's dashboard is simply there". The screen falls back to an explanation
// when the books are too new to have any figures.
const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: function Index() {
    return <DashboardScreen />;
  },
});

const dashboardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/dashboards/$name",
  component: function NamedDashboard() {
    const { name } = useParams({ from: "/dashboards/$name" });
    return <DashboardScreen name={name} />;
  },
});

const listRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/o/$resource",
  component: function List() {
    const { resource } = useParams({ from: "/o/$resource" });
    return <ObjectGate resource={resource}>{(o) => <ObjectList object={o} />}</ObjectGate>;
  },
});

const newRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/o/$resource/new",
  component: function New() {
    const { resource } = useParams({ from: "/o/$resource/new" });
    return <ObjectGate resource={resource}>{(o) => <ObjectForm object={o} />}</ObjectGate>;
  },
});

const detailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/o/$resource/$id",
  component: function Detail() {
    const { resource, id } = useParams({ from: "/o/$resource/$id" });
    return <ObjectGate resource={resource}>{(o) => <ObjectDetail object={o} id={id} />}</ObjectGate>;
  },
});

const editRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/o/$resource/$id/edit",
  component: function Edit() {
    const { resource, id } = useParams({ from: "/o/$resource/$id/edit" });
    return (
      <ObjectGate resource={resource}>
        {(o) => <EditForm object={o} id={id} resource={resource} />}
      </ObjectGate>
    );
  },
});

/** EditForm loads the current values before mounting the form, so the inputs
 * start populated rather than blanking a record on first save. */
function EditForm({
  object,
  id,
  resource,
}: {
  object: Parameters<typeof ObjectForm>[0]["object"];
  id: string;
  resource: string;
}) {
  const t = useT();
  const { data, isPending, error } = useQuery({
    queryKey: ["record", resource, id],
    queryFn: () => getRecord(resource, id),
  });

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("status.error")}</Alert>;
  }
  return <ObjectForm object={object} id={id} initial={data} />;
}

const invoiceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/invoices/$id",
  component: function Invoice() {
    const { id } = useParams({ from: "/invoices/$id" });
    return <InvoiceDetail id={id} />;
  },
});

const routeTree = rootRoute.addChildren([
  indexRoute,
  dashboardRoute,
  listRoute,
  newRoute,
  detailRoute,
  editRoute,
  invoiceRoute,
]);

export function buildRouter(context: RouterContext) {
  return createRouter({ routeTree, context });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof buildRouter>;
  }
}
