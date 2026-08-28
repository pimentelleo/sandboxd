import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentChat } from './AppView'
import { CopilotConversation } from './CopilotConversation'
import { ConversationSnapshot } from './api'

const sandbox = { id: 'sandbox-1', status: 'stopped' }
const emptyConversation: ConversationSnapshot = {
  conversation: null, turns: [], messages: [], interactions: [], children: [], event_cursor: 0, next_queue_slot: 0,
}
const modelCatalog = [{
  id: 'gpt-5.3-codex',
  name: 'GPT-5.3 Codex',
  supported_reasoning_efforts: ['low', 'high'],
  default_reasoning_effort: 'high',
  max_context_window_tokens: 256000,
}, {
  id: 'claude-opus',
  name: 'Claude Opus',
  supported_reasoning_efforts: ['medium'],
}]

class QuietEventSource {
  onmessage: ((event: MessageEvent) => void) | null = null
  onerror: (() => void) | null = null
  close() {}
}

function response(body: unknown) {
  return Promise.resolve(new Response(JSON.stringify(body), { headers: { 'content-type': 'application/json' } }))
}

afterEach(() => vi.unstubAllGlobals())

describe('durable GitHub Copilot chat', () => {
  it('uses the persisted global provider to choose the GitHub Copilot chat', async () => {
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/settings')) return response({ agents: { provider: 'github-copilot' } })
      if (url.endsWith('/conversation')) return response(emptyConversation)
      if (url.endsWith('/agents')) return response({ providers: [] })
      if (url.endsWith('/tasks')) return response({ tasks: [] })
      return response({})
    }))
    render(<AgentChat sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(await screen.findByTestId('copilot-conversation')).toBeTruthy()
    expect(screen.queryByTestId('task-prompt')).toBeNull()
    expect(screen.getByTestId('copilot-prompt')).toBeTruthy()
    expect(screen.queryByTestId('task-agent')).toBeNull()
  })

  it('does not expose a provider picker for a global OpenCode chat', async () => {
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/settings')) return response({ agents: { provider: 'opencode' } })
      if (url.endsWith('/agents')) return response({ providers: [] })
      if (url.endsWith('/tasks')) return response({ tasks: [] })
      return response({})
    }))
    render(<AgentChat sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(await screen.findByTestId('task-prompt')).toBeTruthy()
    expect(screen.queryByTestId('task-agent')).toBeNull()
    expect(screen.getByTestId('task-model')).toBeTruthy()
  })

  it('renders and answers a pending provider input interaction', async () => {
    const snapshot: ConversationSnapshot = {
      conversation: {
        id: 'conversation-1', sandbox_id: sandbox.id, agent: 'github-copilot', state: 'waiting_input',
        default_mode: 'interactive', active_turn_id: 'turn-1', created_at: '', updated_at: '',
      },
      turns: [{ id: 'turn-1', task_id: 'task-1', sequence: 1, prompt: 'Deploy it', mode: 'interactive', context_tier: 'default', status: 'waiting_input', created_at: '' }],
      messages: [{ id: 'message-1', turn_id: 'turn-1', sequence: 1, role: 'user', content: 'Deploy it', status: 'complete', created_at: '' }],
      interactions: [{
        id: 'input-1', turn_id: 'turn-1', sequence: 2, type: 'user_input', status: 'pending',
        question: 'Which region?', choices: ['East US', 'West Europe'], allow_freeform: true, actions: [], created_at: '',
      }],
      children: [],
      event_cursor: 5, next_queue_slot: 0,
    }
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/conversation')) return response(snapshot)
      if (url.endsWith('/answer')) return response({ id: 'input-1', status: 'resolved' })
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('EventSource', QuietEventSource)
    render(<CopilotConversation sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(await screen.findByText('Which region?')).toBeTruthy()
    fireEvent.click(screen.getByTestId('copilot-choice-input-1-0'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      '/v1/sandboxes/sandbox-1/conversation/interactions/input-1/answer',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ answer: 'East US' }) }),
    ))
  })

  it('filters model controls and snapshots their selected values with the message', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/conversation')) return response(emptyConversation)
      if (url.endsWith('/agents/github-copilot/models')) return response({ models: modelCatalog })
      if (url.endsWith('/conversation/messages')) {
        return response({
          id: 'turn-1', task_id: 'task-1', status: 'queued',
          mode: 'interactive', model: 'gpt-5.3-codex', reasoning_effort: 'high',
          context_tier: 'long_context', queue_position: 1,
        })
      }
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<CopilotConversation sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(await screen.findByRole('option', { name: 'GPT-5.3 Codex' })).toBeTruthy()
    fireEvent.change(screen.getByTestId('copilot-model'), { target: { value: 'gpt-5.3-codex' } })
    expect(screen.getByRole('option', { name: 'high' })).toBeTruthy()
    expect(screen.queryByRole('option', { name: 'medium' })).toBeNull()
    expect(screen.getByTestId('copilot-context-hint').textContent).toContain('256k tokens')
    fireEvent.change(screen.getByTestId('copilot-reasoning-effort'), { target: { value: 'high' } })
    fireEvent.change(screen.getByTestId('copilot-context-tier'), { target: { value: 'long_context' } })
    fireEvent.change(screen.getByTestId('copilot-prompt'), { target: { value: 'Build it' } })
    fireEvent.click(screen.getByTestId('copilot-send'))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      '/v1/sandboxes/sandbox-1/conversation/messages',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({
          prompt: 'Build it', mode: 'interactive', model: 'gpt-5.3-codex',
          reasoning_effort: 'high', context_tier: 'long_context',
        }),
      }),
    ))
  })

  it('shows a retryable model-catalog failure without blocking default messages', async () => {
    let catalogRequests = 0
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/conversation')) return response(emptyConversation)
      if (url.endsWith('/agents/github-copilot/models')) {
        catalogRequests++
        if (catalogRequests === 1) {
          return Promise.resolve(new Response(JSON.stringify({ error: { message: 'temporarily unavailable' } }), {
            status: 503, headers: { 'content-type': 'application/json' },
          }))
        }
        return response({ models: modelCatalog })
      }
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<CopilotConversation sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect((await screen.findByTestId('copilot-model-catalog-error')).textContent).toContain('Models are temporarily unavailable.')
    expect((screen.getByTestId('copilot-model') as HTMLSelectElement).disabled).toBe(true)
    fireEvent.click(screen.getByTestId('copilot-model-retry'))
    expect(await screen.findByRole('option', { name: 'GPT-5.3 Codex' })).toBeTruthy()
    expect(screen.queryByTestId('copilot-model-catalog-error')).toBeNull()
  })

  it('renders isolated delegated changes for review and can cancel active work', async () => {
    const snapshot: ConversationSnapshot = {
      conversation: {
        id: 'conversation-1', sandbox_id: sandbox.id, agent: 'github-copilot', state: 'running',
        default_mode: 'interactive', active_turn_id: 'turn-1', created_at: '', updated_at: '',
      },
      turns: [{ id: 'turn-1', task_id: 'task-1', sequence: 1, prompt: 'Build it', mode: 'interactive', context_tier: 'default', status: 'running', created_at: '' }],
      messages: [], interactions: [], event_cursor: 5, next_queue_slot: 0,
      children: [{
        id: 'child-1', parent_turn_id: 'turn-1', label: 'Add tests', task: 'Add focused tests.',
        model: 'gpt-5.3-codex', reasoning_effort: 'high', context_tier: 'long_context',
        status: 'running', patch_state: 'available', changed_files: ['test/app.test.ts'], created_at: '',
      }],
    }
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/conversation')) return response(snapshot)
      if (url.endsWith('/changes/test%2Fapp.test.ts')) {
        return response({
          task_id: 'child-1', path: 'test/app.test.ts', base_sha256: 'abc',
          content: 'test("works", () => {})', deleted: false,
        })
      }
      if (url.endsWith('/children/child-1/cancel')) return response({ ...snapshot.children[0], status: 'cancelling' })
      return response({})
    })
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('EventSource', QuietEventSource)
    render(<CopilotConversation sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(await screen.findByText('Add tests')).toBeTruthy()
    expect(screen.getByText('Changes stay isolated until you review and apply them manually.')).toBeTruthy()
    fireEvent.click(screen.getByTestId('copilot-child-review-child-1-test/app.test.ts'))
    expect(await screen.findByText('test("works", () => {})')).toBeTruthy()
    fireEvent.click(screen.getByTestId('copilot-child-cancel-child-1'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      '/v1/sandboxes/sandbox-1/conversation/children/child-1/cancel',
      expect.objectContaining({ method: 'POST' }),
    ))
  })
})
