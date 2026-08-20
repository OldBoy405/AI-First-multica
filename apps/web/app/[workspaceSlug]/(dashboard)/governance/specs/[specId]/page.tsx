import { SpecTracePage } from "@multica/views/governance/spec-trace";

// AIFIRST: CR-2026-049 TASK-12 — spec trace detail route.
export default async function Page({ params }: { params: Promise<{ specId: string }> }) {
  const { specId } = await params;
  return <SpecTracePage specId={specId} />;
}
