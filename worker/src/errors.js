// ToolError is a problem to report back to Claude as the tool's answer (isError),
// as opposed to a bug in the Worker.
export class ToolError extends Error {}
