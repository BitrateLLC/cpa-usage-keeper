import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { CredentialAliasEditor, isCredentialAliasEditorDisabled } from '../CredentialAliasEditor'

vi.mock('react-i18next', () => ({
  initReactI18next: { type: '3rdParty', init: () => undefined },
  useTranslation: () => ({
    t: (key: string) => key,
  }),
}))

describe('CredentialAliasEditor', () => {
  it('renders the current display name with an edit alias action', () => {
    const html = renderToStaticMarkup(
      <CredentialAliasEditor
        identityId="1"
        displayName="Friendly Auth"
        alias="Friendly Auth"
        saving={false}
        onSaveAlias={async () => undefined}
      />,
    )

    expect(html).toContain('Friendly Auth')
    expect(html).toContain('usage_stats.credentials_alias_edit')
  })

  it('disables other rows while an alias save is in flight', () => {
    expect(isCredentialAliasEditorDisabled('1', false, '')).toBe(false)
    expect(isCredentialAliasEditorDisabled('1', false, '1')).toBe(false)
    expect(isCredentialAliasEditorDisabled('1', false, '2')).toBe(true)
    expect(isCredentialAliasEditorDisabled('1', true, '')).toBe(true)
  })
})
