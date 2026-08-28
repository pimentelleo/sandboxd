import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SettingsView } from './SettingsView'
import { agentsFixture, settingsFixture } from './test/fixtures'

function response(body: unknown) {
  return Promise.resolve(new Response(JSON.stringify(body), { headers: { 'content-type': 'application/json' } }))
}

afterEach(() => vi.unstubAllGlobals())

describe('global agent provider setting', () => {
  it('persists the provider selected in Settings', async () => {
    const updated = {
      ...settingsFixture,
      agents: { ...settingsFixture.agents, provider: 'github-copilot' },
    }
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method || 'GET'
      if (url.endsWith('/v1/settings') && method === 'GET') return response(settingsFixture)
      if (url.endsWith('/v1/settings') && method === 'PATCH') return response(updated)
      if (url.endsWith('/v1/agents')) return response({ providers: agentsFixture })
      if (url.endsWith('/v1/git-credentials')) return response({ credentials: [] })
      if (url.endsWith('/v1/api-keys')) return response({ keys: [] })
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<SettingsView onError={vi.fn()} toast={vi.fn()} />)

    expect(await screen.findByTestId('settings-agent-provider')).toBeTruthy()
    fireEvent.change(screen.getByTestId('settings-agent-provider-select'), { target: { value: 'github-copilot' } })
    fireEvent.click(screen.getByTestId('save-agent-provider'))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      '/v1/settings',
      expect.objectContaining({
        method: 'PATCH',
        body: JSON.stringify({ agents: { provider: 'github-copilot' } }),
      }),
    ))
  })
})
