<script setup lang="ts">
import { useQuery } from '@tanstack/vue-query'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { useApiClient } from '@shared/http/client-context'
import type { TurnStateWatcherConfigDto } from '@/api/control/types'
import { credentialCollectionQueryOptions } from '@/app/resources/credentials'
import { groupOptionsQueryOptions } from '@/app/resources/groups'
import AppButton from '@/components/ui/AppButton.vue'
import AppSelect from '@/components/ui/AppSelect.vue'
import AppSwitch from '@/components/ui/AppSwitch.vue'
import DegradationNumberField from '@/features/monitor/DegradationNumberField.vue'
import FormField from '@/components/ui/FormField.vue'

const props = defineProps<{
  /** 已保存的 Web 配置；null 表示未在 Web 端配置。 */
  config: TurnStateWatcherConfigDto | null
  /** 当前编辑值；null 表示回到未配置状态。 */
  draft: TurnStateWatcherConfigDto | null
  disabled: boolean
  resetKey: number
}>()
const emit = defineEmits<{
  change: [value: TurnStateWatcherConfigDto | null]
  'update:valid': [valid: boolean]
}>()
const { t } = useI18n()
const client = useApiClient()

const defaultLengths = { healthy: '292, 332', degraded: '312, 356' } as const
const maxDurationMilliseconds = 9_223_372_036_854
const maxDurationSeconds = 9_223_372_036

interface TurnStateFormState {
  enabled: boolean
  groupId: string
  credentialId: string
  pushModels: string
  healthyLengths: string
  degradedLengths: string
  pushMaxAgeMs: string
  pollInterval: string
  verifyInterval: string
  verifyTimeout: string
  verifyMax: string
  degradeProxyUrl: string
  verbose: boolean
}

function emptyNumber(value: number): string {
  return Number.isFinite(value) ? String(value) : ''
}

function buildFormState(config: TurnStateWatcherConfigDto): TurnStateFormState {
  return {
    enabled: config.enabled,
    groupId: String(config.group_id),
    credentialId: String(config.credential_id),
    pushModels: config.push_models,
    healthyLengths:
      config.healthy_lengths.length > 0
        ? config.healthy_lengths.join(', ')
        : defaultLengths.healthy,
    degradedLengths:
      config.degraded_lengths.length > 0
        ? config.degraded_lengths.join(', ')
        : defaultLengths.degraded,
    pushMaxAgeMs: emptyNumber(config.push_max_age_ms),
    pollInterval: emptyNumber(config.poll_interval_seconds),
    verifyInterval: emptyNumber(config.verify_interval_seconds),
    verifyTimeout: emptyNumber(config.verify_timeout_seconds),
    verifyMax: emptyNumber(config.verify_max_attempts),
    degradeProxyUrl: config.degrade_proxy_url,
    verbose: config.verbose,
  }
}

// 未配置时的展示基线：与后端 ParseTurnStateWatcherConfig 的默认值一致。
const defaultForm = (): TurnStateFormState => ({
  enabled: false,
  groupId: '1',
  credentialId: '1',
  pushModels: '',
  healthyLengths: defaultLengths.healthy,
  degradedLengths: defaultLengths.degraded,
  pushMaxAgeMs: '3600000',
  pollInterval: '15',
  verifyInterval: '60',
  verifyTimeout: '120',
  verifyMax: '0',
  degradeProxyUrl: '',
  verbose: false,
})

const form = ref<TurnStateFormState>(props.draft ? buildFormState(props.draft) : defaultForm())
watch(
  () => props.resetKey,
  () => {
    form.value = props.draft ? buildFormState(props.draft) : defaultForm()
  },
)

const groupOptionsQuery = useQuery(groupOptionsQueryOptions(client))
const groupSelectOptions = computed(() =>
  (groupOptionsQuery.data.value ?? []).map((group) => ({
    value: String(group.id),
    label: group.name,
  })),
)

const selectedGroupId = computed(() => Number(form.value.groupId) || 0)
const credentialFilters = { page: 1, page_size: 100 } as const
const credentialQuery = useQuery({
  ...credentialCollectionQueryOptions(client, selectedGroupId, () => credentialFilters),
  enabled: () => selectedGroupId.value >= 1,
})
const credentialSelectOptions = computed(() =>
  (credentialQuery.data.value?.items ?? []).map((item) => ({
    value: String(item.credential_id),
    label: `#${item.credential_id} · ${item.mask}`,
  })),
)

function updateField<K extends keyof TurnStateFormState>(key: K, value: TurnStateFormState[K]) {
  form.value[key] = value
  if (key === 'groupId') form.value.credentialId = ''
  emitChange()
}

function parseLengths(text: string): number[] {
  return text
    .split(',')
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '')
    .map((entry) => Number(entry))
}

function emitChange() {
  emit('change', {
    enabled: form.value.enabled,
    group_id: Number(form.value.groupId),
    credential_id: Number(form.value.credentialId),
    push_models: form.value.pushModels.trim(),
    push_max_age_ms: Number(form.value.pushMaxAgeMs),
    healthy_lengths: parseLengths(form.value.healthyLengths),
    degraded_lengths: parseLengths(form.value.degradedLengths),
    poll_interval_seconds: Number(form.value.pollInterval),
    degrade_proxy_mode: form.value.degradeProxyUrl.trim() ? 'custom' : '',
    degrade_proxy_url: form.value.degradeProxyUrl.trim(),
    verify_interval_seconds: Number(form.value.verifyInterval),
    verify_timeout_seconds: Number(form.value.verifyTimeout),
    verify_max_attempts: Number(form.value.verifyMax),
    verbose: form.value.verbose,
  })
}

const modelError = computed(() => {
  if (!form.value.enabled) return undefined
  const model = form.value.pushModels.trim()
  if (!model) return t('settings.turnState.errors.modelRequired')
  if (/[,*]/.test(model)) return t('settings.turnState.errors.modelSingle')
  return undefined
})
const healthyLengthError = computed(() =>
  lengthsError(form.value.healthyLengths, 'settings.turnState.errors.healthyLengths'),
)
const degradedLengthError = computed(() =>
  lengthsError(form.value.degradedLengths, 'settings.turnState.errors.degradedLengths'),
)
const overlapError = computed(() => {
  const healthy = parseLengths(form.value.healthyLengths)
  const degraded = parseLengths(form.value.degradedLengths)
  if (healthy.some((length) => degraded.includes(length))) {
    return t('settings.turnState.errors.lengthsOverlap')
  }
  return undefined
})

function lengthsError(text: string, requiredKey: string): string | undefined {
  const values = text
    .split(',')
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '')
  if (values.length === 0) return t(requiredKey)
  if (values.some((entry) => !Number.isSafeInteger(Number(entry)) || Number(entry) < 1)) {
    return t('settings.turnState.errors.positiveList')
  }
  return undefined
}

const positiveError = computed(() =>
  [
    [form.value.pushMaxAgeMs, maxDurationMilliseconds],
    [form.value.pollInterval, maxDurationSeconds],
    [form.value.verifyInterval, maxDurationSeconds],
    [form.value.verifyTimeout, maxDurationSeconds],
  ].some(
    ([text, maximum]) =>
      !Number.isSafeInteger(Number(text)) || Number(text) < 1 || Number(text) > Number(maximum),
  )
    ? t('settings.turnState.errors.positiveNumber')
    : undefined,
)
const verifyMaxError = computed(() =>
  !Number.isSafeInteger(Number(form.value.verifyMax)) || Number(form.value.verifyMax) < 0
    ? t('settings.turnState.errors.nonNegativeNumber')
    : undefined,
)
const proxyError = computed(() => {
  const url = form.value.degradeProxyUrl.trim()
  if (url && !/^(http|socks5):\/\/\S+$/iu.test(url)) {
    return t('settings.turnState.errors.proxyUrl')
  }
  return undefined
})
const bindingError = computed(() => {
  if (selectedGroupId.value < 1 || Number(form.value.credentialId) < 1) {
    return t('settings.turnState.errors.bindingRequired')
  }
  if (!form.value.enabled) return undefined
  if (
    credentialQuery.isPending.value ||
    !credentialSelectOptions.value.some((option) => option.value === form.value.credentialId)
  ) {
    return t('settings.turnState.errors.bindingRequired')
  }
  return undefined
})

const isValid = computed(
  () =>
    !modelError.value &&
    !healthyLengthError.value &&
    !degradedLengthError.value &&
    !overlapError.value &&
    !positiveError.value &&
    !verifyMaxError.value &&
    !proxyError.value &&
    !bindingError.value,
)
watch(isValid, (valid) => emit('update:valid', valid), { immediate: true })
</script>

<template>
  <section id="settings-turn-state" class="settings-section" tabindex="-1">
    <header class="settings-section__heading">
      <h2>{{ t('settings.turnState.title') }}</h2>
      <p>{{ t('settings.turnState.description') }}</p>
    </header>

    <p class="settings-section__hint">
      {{ config ? t('settings.turnState.webConfigured') : t('settings.turnState.envFallback') }}
    </p>

    <div class="settings-turn-state__grid">
      <div class="settings-turn-state__switch-row">
        <AppSwitch
          :model-value="form.enabled"
          :label="t('settings.turnState.enabled')"
          :disabled="disabled"
          @update:model-value="updateField('enabled', $event)"
        />
        <div>
          <strong>{{ t('settings.turnState.enabled') }}</strong>
          <span>{{ t('settings.turnState.enabledHelp') }}</span>
        </div>
      </div>

      <FormField
        id="settings-turn-state-group"
        :label="t('settings.turnState.group')"
        :description="t('settings.turnState.groupHelp')"
        :error="bindingError"
        size="compact"
      >
        <template #default="{ describedBy }">
          <AppSelect
            id="settings-turn-state-group"
            :model-value="form.groupId"
            :label="t('settings.turnState.group')"
            :options="groupSelectOptions"
            :disabled="disabled"
            :aria-describedby="describedBy"
            @update:model-value="updateField('groupId', $event)"
          />
        </template>
      </FormField>

      <FormField
        id="settings-turn-state-credential"
        :label="t('settings.turnState.credential')"
        :description="t('settings.turnState.credentialHelp')"
        size="compact"
      >
        <template #default="{ describedBy }">
          <AppSelect
            id="settings-turn-state-credential"
            :model-value="form.credentialId"
            :label="t('settings.turnState.credential')"
            :options="credentialSelectOptions"
            :disabled="disabled || selectedGroupId < 1"
            :aria-describedby="describedBy"
            @update:model-value="updateField('credentialId', $event)"
          />
        </template>
      </FormField>

      <FormField
        id="settings-turn-state-model"
        :label="t('settings.turnState.model')"
        :description="t('settings.turnState.modelHelp')"
        :error="modelError"
        size="compact"
      >
        <template #default="{ describedBy, invalid }">
          <input
            id="settings-turn-state-model"
            :value="form.pushModels"
            autocomplete="off"
            :spellcheck="false"
            :disabled="disabled"
            :aria-describedby="describedBy"
            :aria-invalid="invalid || undefined"
            @input="updateField('pushModels', ($event.target as HTMLInputElement).value)"
          />
        </template>
      </FormField>

      <FormField
        id="settings-turn-state-healthy"
        :label="t('settings.turnState.healthyLengths')"
        :description="t('settings.turnState.healthyLengthsHelp')"
        :error="healthyLengthError || overlapError"
        size="compact"
      >
        <template #default="{ describedBy, invalid }">
          <input
            id="settings-turn-state-healthy"
            :value="form.healthyLengths"
            autocomplete="off"
            :spellcheck="false"
            :disabled="disabled"
            :aria-describedby="describedBy"
            :aria-invalid="invalid || undefined"
            @input="updateField('healthyLengths', ($event.target as HTMLInputElement).value)"
          />
        </template>
      </FormField>

      <FormField
        id="settings-turn-state-degraded"
        :label="t('settings.turnState.degradedLengths')"
        :description="t('settings.turnState.degradedLengthsHelp')"
        :error="degradedLengthError"
        size="compact"
      >
        <template #default="{ describedBy, invalid }">
          <input
            id="settings-turn-state-degraded"
            :value="form.degradedLengths"
            autocomplete="off"
            :spellcheck="false"
            :disabled="disabled"
            :aria-describedby="describedBy"
            :aria-invalid="invalid || undefined"
            @input="updateField('degradedLengths', ($event.target as HTMLInputElement).value)"
          />
        </template>
      </FormField>

      <DegradationNumberField
        id="settings-turn-state-max-age"
        :label="t('settings.turnState.pushMaxAge')"
        :description="t('settings.turnState.pushMaxAgeHelp')"
        suffix="ms"
        :model-value="form.pushMaxAgeMs"
        :error="positiveError"
        :disabled="disabled"
        @update:model-value="updateField('pushMaxAgeMs', $event)"
      />

      <DegradationNumberField
        id="settings-turn-state-poll"
        :label="t('settings.turnState.pollInterval')"
        suffix="s"
        :model-value="form.pollInterval"
        :error="positiveError"
        :disabled="disabled"
        @update:model-value="updateField('pollInterval', $event)"
      />

      <DegradationNumberField
        id="settings-turn-state-verify-interval"
        :label="t('settings.turnState.verifyInterval')"
        suffix="s"
        :model-value="form.verifyInterval"
        :error="positiveError"
        :disabled="disabled"
        @update:model-value="updateField('verifyInterval', $event)"
      />

      <DegradationNumberField
        id="settings-turn-state-verify-timeout"
        :label="t('settings.turnState.verifyTimeout')"
        suffix="s"
        :model-value="form.verifyTimeout"
        :error="positiveError"
        :disabled="disabled"
        @update:model-value="updateField('verifyTimeout', $event)"
      />

      <DegradationNumberField
        id="settings-turn-state-verify-max"
        :label="t('settings.turnState.verifyMax')"
        :description="t('settings.turnState.verifyMaxHelp')"
        :model-value="form.verifyMax"
        :error="verifyMaxError"
        :disabled="disabled"
        @update:model-value="updateField('verifyMax', $event)"
      />

      <FormField
        id="settings-turn-state-proxy"
        class="settings-turn-state__wide"
        :label="t('settings.turnState.degradeProxy')"
        :description="t('settings.turnState.degradeProxyHelp')"
        :error="proxyError"
        size="compact"
      >
        <template #default="{ describedBy, invalid }">
          <input
            id="settings-turn-state-proxy"
            :value="form.degradeProxyUrl"
            autocomplete="off"
            :spellcheck="false"
            :disabled="disabled"
            :aria-describedby="describedBy"
            :aria-invalid="invalid || undefined"
            @input="updateField('degradeProxyUrl', ($event.target as HTMLInputElement).value)"
          />
        </template>
      </FormField>

      <div class="settings-turn-state__switch-row">
        <AppSwitch
          :model-value="form.verbose"
          :label="t('settings.turnState.verbose')"
          :disabled="disabled"
          @update:model-value="updateField('verbose', $event)"
        />
        <div>
          <strong>{{ t('settings.turnState.verbose') }}</strong>
          <span>{{ t('settings.turnState.verboseHelp') }}</span>
        </div>
      </div>
    </div>

    <div v-if="config" class="settings-turn-state__clear">
      <AppButton variant="ghost" size="sm" :disabled="disabled" @click="emit('change', null)">
        {{ t('settings.turnState.clearToEnv') }}
      </AppButton>
    </div>
  </section>
</template>

<style scoped>
.settings-section {
  display: grid;
  gap: var(--space-4);
  scroll-margin-top: 76px;
}

.settings-section__heading {
  display: grid;
}

.settings-section__heading h2,
.settings-section__heading p {
  margin: 0;
}

.settings-section__heading h2 {
  font-size: var(--title-section);
  font-weight: 650;
}

.settings-section__heading p {
  margin-top: var(--space-1);
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}

.settings-section__hint {
  margin: 0;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}

.settings-turn-state__grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-3) var(--space-4);
}

.settings-turn-state__switch-row {
  grid-column: 1 / -1;
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.settings-turn-state__switch-row > div {
  display: grid;
  gap: 2px;
}

.settings-turn-state__switch-row strong {
  font-size: var(--text-sm);
}

.settings-turn-state__switch-row span {
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}

.settings-turn-state__wide {
  grid-column: 1 / -1;
}

.settings-turn-state__clear {
  display: flex;
  justify-content: flex-start;
}

.settings-turn-state__grid :deep(input) {
  width: 100%;
}

@media (max-width: 720px) {
  .settings-turn-state__grid {
    grid-template-columns: 1fr;
  }
}
</style>
