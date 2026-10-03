// Mirrors the JSON contract served by internal/api (see ROADMAP.md, API contract).

export type RunStatus = 'ok' | 'partial' | 'error';

export interface RunSummary {
  id: string;
  job: string;
  trigger: string;
  started_at: string;
  ended_at: string;
  status: RunStatus;
  stop?: string;
  model?: string;
  steps: number;
  tool_calls: number;
  prompt_tokens: number;
  completion_tokens: number;
  cost_usd: number;
  delivered: boolean;
  error?: string;
  preview?: string;
}

export interface ToolCall {
  id: string;
  name: string;
  arguments: string;
}

export interface ChatMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: string;
  tool_calls?: ToolCall[];
  tool_call_id?: string;
}

export interface RunRecord extends RunSummary {
  output: string;
  messages: ChatMessage[];
}

export interface JobView {
  name: string;
  enabled: boolean;
  schedule: string;
  model: string;
  budget_usd: number;
  running: boolean;
  next_run: string | null;
  last_run: RunSummary | null;
  cost_7d: number;
}

export interface McpView {
  name: string;
  transport: 'stdio' | 'http';
  target: string;
  allow: string[];
  header_keys: string[];
  env_keys: string[];
}

export interface SinkView {
  type: string;
  option_keys: string[];
}

export interface JobDetail extends JobView {
  fallbacks: string[];
  max_steps: number;
  prompt: string;
  mcp: McpView[];
  sinks: SinkView[];
  memory: { file: string; max_kb: number };
}

export interface Health {
  version: string;
  uptime_seconds: number;
}

export interface MemoryView {
  file: string;
  content: string;
}
