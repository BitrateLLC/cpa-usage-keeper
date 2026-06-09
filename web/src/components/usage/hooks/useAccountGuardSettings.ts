import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, fetchAccountGuardSettings, updateAccountGuardSettings } from '@/lib/api';
import type { AccountGuardSettings, AccountGuardSettingsInput } from '@/lib/types';

export interface UseAccountGuardSettingsOptions {
  onAuthRequired?: () => void;
  enabled?: boolean;
}

export interface UseAccountGuardSettingsReturn {
  settings: AccountGuardSettings | null;
  loading: boolean;
  saving: boolean;
  error: string;
  reload: () => Promise<void>;
  save: (input: AccountGuardSettingsInput) => Promise<boolean>;
}

export function useAccountGuardSettings(options: UseAccountGuardSettingsOptions = {}): UseAccountGuardSettingsReturn {
  const { onAuthRequired, enabled = true } = options;
  const [settings, setSettings] = useState<AccountGuardSettings | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const controllerRef = useRef<AbortController | null>(null);
  const onAuthRequiredRef = useRef(onAuthRequired);

  useEffect(() => {
    onAuthRequiredRef.current = onAuthRequired;
  }, [onAuthRequired]);

  const reload = useCallback(async () => {
    controllerRef.current?.abort();
    const controller = new AbortController();
    controllerRef.current = controller;
    setLoading(true);
    setError('');
    try {
      const response = await fetchAccountGuardSettings(controller.signal);
      if (controllerRef.current !== controller) return;
      setSettings(response);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequiredRef.current?.();
        return;
      }
      setError(err instanceof Error ? err.message : 'Failed to load account guard settings');
    } finally {
      if (controllerRef.current === controller) {
        setLoading(false);
        controllerRef.current = null;
      }
    }
  }, []);

  useEffect(() => {
    if (!enabled) {
      controllerRef.current?.abort();
      controllerRef.current = null;
      setLoading(false);
      return;
    }
    void reload();
    return () => {
      controllerRef.current?.abort();
      controllerRef.current = null;
    };
  }, [enabled, reload]);

  const save = useCallback(async (input: AccountGuardSettingsInput): Promise<boolean> => {
    setSaving(true);
    try {
      const response = await updateAccountGuardSettings(input);
      setSettings(response);
      return true;
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        onAuthRequiredRef.current?.();
        return false;
      }
      throw err;
    } finally {
      setSaving(false);
    }
  }, []);

  return { settings, loading, saving, error, reload, save };
}
