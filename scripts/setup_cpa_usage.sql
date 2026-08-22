-- CPA usage sync schema (matches CLIProxyAPI supabase_sync.go contract)

create table if not exists public.cpa_usage_daily (
  day date not null,
  provider text not null default '',
  supplier text not null default '',
  auth_hash text not null default '',
  model text not null default '',
  protocol text not null default '',
  schema_version integer not null default 1,
  requests bigint not null default 0,
  success bigint not null default 0,
  failed bigint not null default 0,
  rate_limited bigint not null default 0,
  excluded_client_faults bigint not null default 0,
  input_tokens bigint not null default 0,
  output_tokens bigint not null default 0,
  total_tokens bigint not null default 0,
  cached_tokens bigint not null default 0,
  latency_ms_sum double precision not null default 0,
  latency_count bigint not null default 0,
  ttft_ms_sum double precision not null default 0,
  ttft_count bigint not null default 0,
  generation_output_tokens bigint not null default 0,
  generation_seconds double precision not null default 0,
  first_seen_at timestamptz not null default now(),
  last_occurred_at timestamptz,
  updated_at timestamptz not null default now(),
  primary key (day, provider, supplier, auth_hash, model, protocol)
);

create table if not exists public.cpa_usage_batches (
  idempotency_key text primary key,
  schema_version integer,
  occurred_at text,
  payload_sha256 text,
  row_count integer,
  applied_at timestamptz not null default now()
);

alter table public.cpa_usage_daily enable row level security;
alter table public.cpa_usage_batches enable row level security;

create or replace function public.cpa_usage_apply_batch(
  p_idempotency_key text,
  p_schema_version integer,
  p_occurred_at text,
  p_payload_sha256 text,
  p_rows jsonb
) returns void
language plpgsql
security definer
set search_path = public
as $$
declare
  r jsonb;
begin
  if p_idempotency_key is null or p_idempotency_key = '' then
    raise exception 'cpa_usage_apply_batch: missing idempotency key';
  end if;

  if exists (
    select 1 from public.cpa_usage_batches b
    where b.idempotency_key = p_idempotency_key
  ) then
    return;
  end if;

  for r in select value from jsonb_array_elements(coalesce(p_rows, '[]'::jsonb)) loop
    insert into public.cpa_usage_daily as d (
      day, provider, supplier, auth_hash, model, protocol, schema_version,
      requests, success, failed, rate_limited, excluded_client_faults,
      input_tokens, output_tokens, total_tokens, cached_tokens,
      latency_ms_sum, latency_count, ttft_ms_sum, ttft_count,
      generation_output_tokens, generation_seconds, last_occurred_at
    ) values (
      coalesce((nullif(r->>'day', ''))::date, now()::date),
      coalesce(r->>'provider', ''),
      coalesce(r->>'supplier', ''),
      coalesce(r->>'auth_hash', ''),
      coalesce(r->>'model', ''),
      coalesce(r->>'protocol', ''),
      coalesce((r->>'schema_version')::integer, 1),
      coalesce((r->>'requests')::bigint, 0),
      coalesce((r->>'success')::bigint, 0),
      coalesce((r->>'failed')::bigint, 0),
      coalesce((r->>'rate_limited')::bigint, 0),
      coalesce((r->>'excluded_client_faults')::bigint, 0),
      coalesce((r->>'input_tokens')::bigint, 0),
      coalesce((r->>'output_tokens')::bigint, 0),
      coalesce((r->>'total_tokens')::bigint, 0),
      coalesce((r->>'cached_tokens')::bigint, 0),
      coalesce((r->>'latency_ms_sum')::double precision, 0),
      coalesce((r->>'latency_count')::bigint, 0),
      coalesce((r->>'ttft_ms_sum')::double precision, 0),
      coalesce((r->>'ttft_count')::bigint, 0),
      coalesce((r->>'generation_output_tokens')::bigint, 0),
      coalesce((r->>'generation_seconds')::double precision, 0),
      coalesce((nullif(r->>'occurred_at', ''))::timestamptz, now())
    )
    on conflict (day, provider, supplier, auth_hash, model, protocol) do update set
      schema_version = greatest(d.schema_version, excluded.schema_version),
      requests = d.requests + excluded.requests,
      success = d.success + excluded.success,
      failed = d.failed + excluded.failed,
      rate_limited = d.rate_limited + excluded.rate_limited,
      excluded_client_faults = d.excluded_client_faults + excluded.excluded_client_faults,
      input_tokens = d.input_tokens + excluded.input_tokens,
      output_tokens = d.output_tokens + excluded.output_tokens,
      total_tokens = d.total_tokens + excluded.total_tokens,
      cached_tokens = d.cached_tokens + excluded.cached_tokens,
      latency_ms_sum = d.latency_ms_sum + excluded.latency_ms_sum,
      latency_count = d.latency_count + excluded.latency_count,
      ttft_ms_sum = d.ttft_ms_sum + excluded.ttft_ms_sum,
      ttft_count = d.ttft_count + excluded.ttft_count,
      generation_output_tokens = d.generation_output_tokens + excluded.generation_output_tokens,
      generation_seconds = d.generation_seconds + excluded.generation_seconds,
      last_occurred_at = greatest(coalesce(d.last_occurred_at, excluded.last_occurred_at), excluded.last_occurred_at),
      updated_at = now();
  end loop;

  insert into public.cpa_usage_batches (
    idempotency_key, schema_version, occurred_at, payload_sha256, row_count
  ) values (
    p_idempotency_key,
    p_schema_version,
    p_occurred_at,
    p_payload_sha256,
    coalesce(jsonb_array_length(p_rows), 0)
  );
end;
$$;
