import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Select, type SelectOption } from '@/components/ui/Select';
import type {
  AccountGuardChannelType,
  AccountGuardSettings,
  AccountGuardSettingsInput,
} from '@/lib/types';
import styles from '@/pages/UsagePage.module.scss';

// 渠道草稿：密钥字段为只写草稿（''=保持原值），masked/configured 用于占位提示。
interface DraftChannel {
  id: string;
  type: AccountGuardChannelType;
  enabled: boolean;
  telegramBotToken: string;
  telegramBotTokenMasked: string;
  telegramBotTokenConfigured: boolean;
  telegramChatId: string;
  webhookUrl: string;
  webhookUrlMasked: string;
  webhookUrlConfigured: boolean;
}

export interface AccountGuardSettingsCardProps {
  settings: AccountGuardSettings | null;
  loading?: boolean;
  saving?: boolean;
  onSave: (input: AccountGuardSettingsInput) => Promise<boolean>;
  onNotice?: (kind: 'success' | 'info' | 'error', message: string) => void;
}

const newLocalId = (): string => {
  const cryptoRef = typeof crypto !== 'undefined' ? crypto : undefined;
  const random = cryptoRef?.randomUUID ? cryptoRef.randomUUID() : `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
  return `new-${random}`;
};

const settingsToDraftChannels = (settings: AccountGuardSettings | null): DraftChannel[] =>
  (settings?.channels ?? []).map((channel) => ({
    id: channel.id,
    type: channel.type,
    enabled: channel.enabled,
    telegramBotToken: '',
    telegramBotTokenMasked: channel.telegram_bot_token,
    telegramBotTokenConfigured: channel.telegram_bot_token_configured,
    telegramChatId: channel.telegram_chat_id,
    webhookUrl: '',
    webhookUrlMasked: channel.webhook_url,
    webhookUrlConfigured: channel.webhook_url_configured,
  }));

export function AccountGuardSettingsCard({ settings, loading = false, saving = false, onSave, onNotice }: AccountGuardSettingsCardProps) {
  const { t } = useTranslation();

  const [enabled, setEnabled] = useState(false);
  const [intervalSeconds, setIntervalSeconds] = useState('900');
  const [disableCodes, setDisableCodes] = useState<number[]>([]);
  const [monitorCodes, setMonitorCodes] = useState<number[]>([]);
  const [disableCodeInput, setDisableCodeInput] = useState('');
  const [monitorCodeInput, setMonitorCodeInput] = useState('');
  const [channels, setChannels] = useState<DraftChannel[]>([]);

  // 设置加载/保存返回后，用最新值重置草稿。
  useEffect(() => {
    if (!settings) return;
    setEnabled(settings.enabled);
    setIntervalSeconds(String(settings.alert_interval_seconds));
    setDisableCodes(settings.disable_status_codes);
    setMonitorCodes(settings.monitor_status_codes);
    setChannels(settingsToDraftChannels(settings));
  }, [settings]);

  const channelTypeOptions: SelectOption[] = useMemo(
    () => [
      { value: 'telegram', label: t('usage_stats.account_guard_channel_telegram') },
      { value: 'webhook', label: t('usage_stats.account_guard_channel_webhook') },
    ],
    [t],
  );

  const addCode = (raw: string, codes: number[], setCodes: (next: number[]) => void, clear: () => void) => {
    const value = Number(raw.trim());
    if (!Number.isInteger(value) || value < 100 || value > 599) {
      onNotice?.('error', t('usage_stats.account_guard_code_invalid'));
      return;
    }
    if (codes.includes(value)) {
      clear();
      return;
    }
    setCodes([...codes, value].sort((a, b) => a - b));
    clear();
  };

  const removeCode = (code: number, codes: number[], setCodes: (next: number[]) => void) => {
    setCodes(codes.filter((item) => item !== code));
  };

  const updateChannel = (id: string, patch: Partial<DraftChannel>) => {
    setChannels((current) => current.map((channel) => (channel.id === id ? { ...channel, ...patch } : channel)));
  };

  const addChannel = () => {
    setChannels((current) => [
      ...current,
      {
        id: newLocalId(),
        type: 'telegram',
        enabled: true,
        telegramBotToken: '',
        telegramBotTokenMasked: '',
        telegramBotTokenConfigured: false,
        telegramChatId: '',
        webhookUrl: '',
        webhookUrlMasked: '',
        webhookUrlConfigured: false,
      },
    ]);
  };

  const removeChannel = (id: string) => {
    setChannels((current) => current.filter((channel) => channel.id !== id));
  };

  const handleSave = async () => {
    const interval = Number(intervalSeconds.trim());
    if (!Number.isInteger(interval) || interval < 60) {
      onNotice?.('error', t('usage_stats.account_guard_interval_invalid'));
      return;
    }
    const payload: AccountGuardSettingsInput = {
      enabled,
      disable_status_codes: disableCodes,
      monitor_status_codes: monitorCodes,
      alert_interval_seconds: interval,
      channels: channels.map((channel) => ({
        id: channel.id.startsWith('new-') ? '' : channel.id,
        type: channel.type,
        enabled: channel.enabled,
        telegram_bot_token: channel.telegramBotToken,
        telegram_chat_id: channel.telegramChatId,
        webhook_url: channel.webhookUrl,
      })),
    };
    try {
      const ok = await onSave(payload);
      if (ok) {
        onNotice?.('success', t('usage_stats.account_guard_save_success'));
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : '';
      onNotice?.('error', `${t('usage_stats.account_guard_save_failed')}${message ? `: ${message}` : ''}`);
    }
  };

  const renderCodeEditor = (
    titleKey: string,
    codes: number[],
    setCodes: (next: number[]) => void,
    inputValue: string,
    setInputValue: (value: string) => void,
  ) => (
    <div className={styles.guardCodeField}>
      <label>{t(titleKey)}</label>
      <div className={styles.guardCodeInputRow}>
        <Input
          type="number"
          value={inputValue}
          onChange={(e) => setInputValue(e.target.value)}
          placeholder="429"
          className={styles.usagePillControl}
        />
        <Button
          variant="secondary"
          size="sm"
          className={styles.usagePillAction}
          onClick={() => addCode(inputValue, codes, setCodes, () => setInputValue(''))}
        >
          {t('usage_stats.account_guard_code_add')}
        </Button>
      </div>
      <div className={styles.guardCodeChips}>
        {codes.length === 0 ? (
          <span className={styles.hint}>{t('usage_stats.account_guard_code_empty')}</span>
        ) : (
          codes.map((code) => (
            <Button
              key={code}
              variant="ghost"
              size="sm"
              className={styles.usagePillAction}
              onClick={() => removeCode(code, codes, setCodes)}
              title={t('common.delete')}
            >
              {code} ✕
            </Button>
          ))
        )}
      </div>
    </div>
  );

  return (
    <Card
      title={
        <div className={styles.sectionTitleBlock}>
          <h3 className={styles.sectionTitle}>{t('usage_stats.account_guard_title')}</h3>
          <p className={styles.sectionSubtitle}>{t('usage_stats.account_guard_subtitle')}</p>
        </div>
      }
      className={`${styles.detailsFixedCard} ${styles.pricingFixedCard}`}
    >
      <div className={styles.pricingSection}>
        {loading && !settings ? (
          <div className={styles.hint}>{t('common.loading')}</div>
        ) : (
          <>
            <div className={`${styles.priceForm} ${styles.guardFormStack}`}>
              <div className={styles.formRow}>
                <label className={styles.guardToggleField}>
                  <input
                    type="checkbox"
                    checked={enabled}
                    onChange={(e) => setEnabled(e.target.checked)}
                    aria-label={t('usage_stats.account_guard_enabled')}
                  />
                  <span>{t('usage_stats.account_guard_enabled')}</span>
                </label>
                <div className={styles.formField}>
                  <label>{t('usage_stats.account_guard_interval')}</label>
                  <Input
                    type="number"
                    value={intervalSeconds}
                    onChange={(e) => setIntervalSeconds(e.target.value)}
                    min={60}
                    placeholder="900"
                    className={styles.usagePillControl}
                  />
                </div>
              </div>

              <div className={styles.formRow}>
                {renderCodeEditor('usage_stats.account_guard_disable_codes', disableCodes, setDisableCodes, disableCodeInput, setDisableCodeInput)}
                {renderCodeEditor('usage_stats.account_guard_monitor_codes', monitorCodes, setMonitorCodes, monitorCodeInput, setMonitorCodeInput)}
              </div>
            </div>

            <div className={styles.pricesList}>
              <div className={styles.guardChannelsHeader}>
                <h4 className={styles.pricesTitle}>{t('usage_stats.account_guard_channels')}</h4>
                <Button variant="secondary" size="sm" className={styles.usagePillAction} onClick={addChannel}>
                  {t('usage_stats.account_guard_channel_add')}
                </Button>
              </div>
              {channels.length === 0 ? (
                <div className={styles.hint}>{t('usage_stats.account_guard_channel_empty')}</div>
              ) : (
                <div className={styles.pricesGrid}>
                  {channels.map((channel) => (
                    <div key={channel.id} className={styles.guardChannelItem}>
                      <div className={styles.guardChannelBody}>
                        <div className={styles.formRow}>
                          <div className={styles.formField}>
                            <label>{t('usage_stats.account_guard_channel_type')}</label>
                            <Select
                              value={channel.type}
                              options={channelTypeOptions}
                              onChange={(value) => updateChannel(channel.id, { type: value as AccountGuardChannelType })}
                              className={styles.usagePillControl}
                            />
                          </div>
                          <label className={styles.guardToggleField}>
                            <input
                              type="checkbox"
                              checked={channel.enabled}
                              onChange={(e) => updateChannel(channel.id, { enabled: e.target.checked })}
                              aria-label={t('usage_stats.account_guard_channel_enabled')}
                            />
                            <span>{t('usage_stats.account_guard_channel_enabled')}</span>
                          </label>
                        </div>
                        {channel.type === 'telegram' ? (
                          <div className={styles.formRow}>
                            <div className={styles.formField}>
                              <label>{t('usage_stats.account_guard_telegram_token')}</label>
                              <Input
                                value={channel.telegramBotToken}
                                onChange={(e) => updateChannel(channel.id, { telegramBotToken: e.target.value })}
                                placeholder={channel.telegramBotTokenConfigured ? channel.telegramBotTokenMasked : t('usage_stats.account_guard_secret_placeholder')}
                                className={styles.usagePillControl}
                              />
                            </div>
                            <div className={styles.formField}>
                              <label>{t('usage_stats.account_guard_telegram_chat_id')}</label>
                              <Input
                                value={channel.telegramChatId}
                                onChange={(e) => updateChannel(channel.id, { telegramChatId: e.target.value })}
                                placeholder="123456789"
                                className={styles.usagePillControl}
                              />
                            </div>
                          </div>
                        ) : (
                          <div className={styles.formField}>
                            <label>{t('usage_stats.account_guard_webhook_url')}</label>
                            <Input
                              value={channel.webhookUrl}
                              onChange={(e) => updateChannel(channel.id, { webhookUrl: e.target.value })}
                              placeholder={channel.webhookUrlConfigured ? channel.webhookUrlMasked : t('usage_stats.account_guard_secret_placeholder')}
                              className={styles.usagePillControl}
                            />
                          </div>
                        )}
                      </div>
                      <div className={styles.guardChannelActions}>
                        <Button
                          variant="danger"
                          size="sm"
                          className={`${styles.usagePillAction} ${styles.usagePillActionDanger}`}
                          onClick={() => removeChannel(channel.id)}
                        >
                          {t('common.delete')}
                        </Button>
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </div>

            <div className={styles.priceActions}>
              <Button variant="primary" className={styles.usagePillAction} onClick={() => void handleSave()} disabled={saving}>
                {saving ? t('common.loading') : t('common.save')}
              </Button>
            </div>
          </>
        )}
      </div>
    </Card>
  );
}
