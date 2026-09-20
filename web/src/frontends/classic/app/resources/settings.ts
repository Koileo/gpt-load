import { queryOptions } from '@tanstack/vue-query'
import { computed, toValue, type MaybeRefOrGetter } from 'vue'

import type { ApiClient } from '@shared/http/client'
import {
  routeStrategies,
  type ProxyMutation,
  type ProxyViewDto,
  type RouteStrategy,
  type TurnStateWatcherConfigDto,
} from '@/api/control/types'
import { InvalidResponseError } from '@shared/http/errors'
import { controlQueryKeys } from '@/app/query-keys'

import type { HeaderRulesDto } from './groups'
import {
  assertNoSecretLikeFields,
  projectArray,
  projectBoolean,
  projectEnum,
  projectRecord,
  projectSafeInteger,
  projectString,
} from './projector'
import { projectProxyView } from './proxy'

export const runtimeSettingKeys = [
  'route_strategy',
  'first_byte_timeout',
  'request_timeout',
  'stream_idle_timeout',
  'retry_count',
  'blacklist_threshold',
  'header_rules',
  'cors',
  'response_header_rules',
  'affinity_enabled',
  'responses_websocket_enabled',
  'affinity_ttl',
  'affinity_capacity',
  'validation_interval',
  'request_log_retention_days',
  'models_dev_auto_sync_enabled',
] as const

export type RuntimeSettingKey = (typeof runtimeSettingKeys)[number]
export type TimeoutSettingKey = Exclude<
  RuntimeSettingKey,
  | 'route_strategy'
  | 'retry_count'
  | 'blacklist_threshold'
  | 'header_rules'
  | 'cors'
  | 'response_header_rules'
  | 'affinity_enabled'
  | 'responses_websocket_enabled'
  | 'affinity_capacity'
  | 'request_log_retention_days'
  | 'models_dev_auto_sync_enabled'
>
export type PolicyCountSettingKey = 'retry_count' | 'blacklist_threshold'

export interface CORSConfigDto {
  enabled: boolean
  allowed_origins: string[]
  allowed_methods: string[]
  allowed_headers: string[]
  exposed_headers: string[]
  allow_credentials: boolean
  max_age: number
}

export interface SettingsValues {
  route_strategy: RouteStrategy
  first_byte_timeout: number
  request_timeout: number
  stream_idle_timeout: number
  retry_count: number
  blacklist_threshold: number
  header_rules: HeaderRulesDto
  cors: CORSConfigDto
  response_header_rules: HeaderRulesDto
  affinity_enabled: boolean
  responses_websocket_enabled: boolean
  affinity_ttl: number
  affinity_capacity: number
  validation_interval: number
  request_log_retention_days: number
  models_dev_auto_sync_enabled: boolean
  proxy_config: ProxyViewDto
  turn_state_watcher: TurnStateWatcherConfigDto | null
}

export interface SettingsDto {
  values: SettingsValues
  overrides: RuntimeSettingKey[]
  read_only: RuntimeSettingKey[]
}

export type SettingsPatch = Partial<{
  route_strategy: RouteStrategy | null
  first_byte_timeout: number | null
  request_timeout: number | null
  stream_idle_timeout: number | null
  retry_count: number | null
  blacklist_threshold: number | null
  header_rules: HeaderRulesDto | null
  cors: CORSConfigDto | null
  response_header_rules: HeaderRulesDto | null
  affinity_enabled: boolean | null
  responses_websocket_enabled: boolean | null
  affinity_ttl: number | null
  affinity_capacity: number | null
  validation_interval: number | null
  request_log_retention_days: number | null
  models_dev_auto_sync_enabled: boolean | null
  proxy_config: ProxyMutation
  turn_state_watcher: TurnStateWatcherConfigDto | null
}>

export interface SettingsResource {
  settings: SettingsDto
}

const settingsFields = ['values', 'overrides', 'read_only'] as const
const settingsValueFields = [...runtimeSettingKeys, 'proxy_config', 'turn_state_watcher'] as const

const turnStateWatcherFields = [
  'enabled',
  'group_id',
  'credential_id',
  'push_models',
  'push_max_age_ms',
  'healthy_lengths',
  'degraded_lengths',
  'poll_interval_seconds',
  'degrade_proxy_mode',
  'degrade_proxy_url',
  'verify_interval_seconds',
  'verify_timeout_seconds',
  'verify_max_attempts',
  'verbose',
] as const

function projectTurnStateWatcher(value: unknown): TurnStateWatcherConfigDto | null {
  if (value === null) return null
  const record = projectRecord(value)
  assertNoSecretLikeFields(record, [...turnStateWatcherFields])
  return {
    enabled: projectBoolean(record.enabled),
    group_id: projectSafeInteger(record.group_id, { minimum: 0 }),
    credential_id: projectSafeInteger(record.credential_id, { minimum: 0 }),
    push_models: projectString(record.push_models, { allowEmpty: true }),
    push_max_age_ms: projectSafeInteger(record.push_max_age_ms, { minimum: 1 }),
    healthy_lengths: projectArray(record.healthy_lengths, (length) =>
      projectSafeInteger(length, { minimum: 1 }),
    ),
    degraded_lengths: projectArray(record.degraded_lengths, (length) =>
      projectSafeInteger(length, { minimum: 1 }),
    ),
    poll_interval_seconds: projectSafeInteger(record.poll_interval_seconds, { minimum: 1 }),
    degrade_proxy_mode: projectString(record.degrade_proxy_mode, { allowEmpty: true }),
    degrade_proxy_url: projectString(record.degrade_proxy_url, { allowEmpty: true }),
    verify_interval_seconds: projectSafeInteger(record.verify_interval_seconds, { minimum: 1 }),
    verify_timeout_seconds: projectSafeInteger(record.verify_timeout_seconds, { minimum: 1 }),
    verify_max_attempts: projectSafeInteger(record.verify_max_attempts, { minimum: 0 }),
    verbose: projectBoolean(record.verbose),
  }
}

function invalidResponse(): never {
  throw new InvalidResponseError()
}

function projectHeaderRules(value: unknown): HeaderRulesDto {
  const record = projectRecord(value)
  assertNoSecretLikeFields(record, ['set', 'remove'])
  const setRecord = projectRecord(record.set)
  const set: Record<string, string> = {}
  for (const [name, headerValue] of Object.entries(setRecord)) {
    if (name.trim().length === 0 || name !== name.trim()) invalidResponse()
    set[name] = projectString(headerValue, { allowEmpty: true })
  }
  return {
    set,
    remove: projectArray(record.remove, (name) => {
      const projected = projectString(name)
      if (projected.trim().length === 0 || projected !== projected.trim()) invalidResponse()
      return projected
    }),
  }
}

function projectCORSConfig(value: unknown): CORSConfigDto {
  const record = projectRecord(value)
  assertNoSecretLikeFields(record, [
    'enabled',
    'allowed_origins',
    'allowed_methods',
    'allowed_headers',
    'exposed_headers',
    'allow_credentials',
    'max_age',
  ])
  const projectList = (input: unknown): string[] =>
    projectArray(input, (item) => {
      const projected = projectString(item)
      if (projected !== projected.trim()) invalidResponse()
      return projected
    })
  return {
    enabled: projectBoolean(record.enabled),
    allowed_origins: projectList(record.allowed_origins),
    allowed_methods: projectList(record.allowed_methods),
    allowed_headers: projectList(record.allowed_headers),
    exposed_headers: projectList(record.exposed_headers),
    allow_credentials: projectBoolean(record.allow_credentials),
    max_age: projectSafeInteger(record.max_age, { minimum: 0 }),
  }
}

export function projectSettings(value: unknown): SettingsDto {
  const record = projectRecord(value)
  assertNoSecretLikeFields(record, settingsFields)
  const values = projectRecord(record.values)
  assertNoSecretLikeFields(values, settingsValueFields)
  const overrides = projectArray(record.overrides, (key) => {
    const projected = projectString(key)
    if (!runtimeSettingKeys.includes(projected as RuntimeSettingKey)) invalidResponse()
    return projected as RuntimeSettingKey
  })
  if (new Set(overrides).size !== overrides.length) invalidResponse()
  const readOnly =
    record.read_only === undefined
      ? []
      : projectArray(record.read_only, (key) => {
          const projected = projectString(key)
          if (!runtimeSettingKeys.includes(projected as RuntimeSettingKey)) invalidResponse()
          return projected as RuntimeSettingKey
        })
  if (new Set(readOnly).size !== readOnly.length) invalidResponse()

  return {
    values: {
      route_strategy: projectEnum(values.route_strategy, routeStrategies),
      first_byte_timeout: projectSafeInteger(values.first_byte_timeout, { minimum: 1 }),
      request_timeout: projectSafeInteger(values.request_timeout, { minimum: 1 }),
      stream_idle_timeout: projectSafeInteger(values.stream_idle_timeout, { minimum: 1 }),
      retry_count: projectSafeInteger(values.retry_count, { minimum: 0 }),
      blacklist_threshold: projectSafeInteger(values.blacklist_threshold, { minimum: 0 }),
      header_rules: projectHeaderRules(values.header_rules),
      cors: projectCORSConfig(values.cors),
      response_header_rules: projectHeaderRules(values.response_header_rules),
      affinity_enabled: projectBoolean(values.affinity_enabled),
      responses_websocket_enabled: projectBoolean(values.responses_websocket_enabled),
      affinity_ttl: projectSafeInteger(values.affinity_ttl, { minimum: 1 }),
      affinity_capacity: projectSafeInteger(values.affinity_capacity, {
        minimum: 1,
        maximum: 1_000_000,
      }),
      validation_interval: projectSafeInteger(values.validation_interval, { minimum: 1 }),
      request_log_retention_days: projectSafeInteger(values.request_log_retention_days, {
        minimum: 1,
        maximum: 365,
      }),
      models_dev_auto_sync_enabled: projectBoolean(values.models_dev_auto_sync_enabled),
      proxy_config: projectProxyView(values.proxy_config),
      turn_state_watcher: projectTurnStateWatcher(values.turn_state_watcher),
    },
    overrides,
    read_only: readOnly,
  }
}

export function settingsQueryIdentity(locale: string) {
  return controlQueryKeys.settings(locale)
}

export async function getSettings(
  client: ApiClient,
  signal?: AbortSignal,
): Promise<SettingsResource> {
  return { settings: projectSettings(await client.request<unknown>('/api/settings', { signal })) }
}

export function settingsQueryOptions(client: ApiClient, locale: MaybeRefOrGetter<string>) {
  return queryOptions({
    queryKey: computed(() => settingsQueryIdentity(toValue(locale))),
    queryFn: ({ signal }) => getSettings(client, signal),
    gcTime: 0,
  })
}

export async function updateSettings(
  client: ApiClient,
  patch: SettingsPatch,
  signal?: AbortSignal,
): Promise<SettingsResource> {
  return {
    settings: projectSettings(
      await client.request<unknown>('/api/settings', {
        method: 'PUT',
        json: { settings: patch },
        signal,
      }),
    ),
  }
}
