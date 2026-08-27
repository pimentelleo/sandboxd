import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentChat } from './AppView'
import { CopilotConversation } from './CopilotConversation'
import { ConversationSnapshot } from './api'

const sandbox = { id: 'sandbox-1', status: 'stopped' }
const emptyConversation: ConversationSnapshot = {
  conversation: null, turns: [], messages: [], interactions: [], event_cursor: 0, next_queue_slot: 0,
}

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
  it('replaces the legacy task chat only after GitHub Copilot is selected', async () => {
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/conversation')) return response(emptyConversation)
      if (url.endsWith('/agents')) return response({ providers: [] })
      if (url.endsWith('/tasks')) return response({ tasks: [] })
      return response({})
    }))
    render(<AgentChat sb={sandbox} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(screen.getByTestId('task-prompt')).toBeTruthy()
    fireEvent.change(screen.getByTestId('task-agent'), { target: { value: 'github-copilot' } })

    expect(await screen.findByTestId('copilot-conversation')).toBeTruthy()
    expect(screen.queryByTestId('task-prompt')).toBeNull()
    expect(screen.getByTestId('copilot-prompt')).toBeTruthy()
  })

  it('renders and answers a pending provider input interaction', async () => {
    const snapshot: ConversationSnapshot = {
      conversation: {
        id: 'conversation-1', sandbox_id: sandbox.id, agent: 'github-copilot', state: 'waiting_input',
        default_mode: 'interactive', active_turn_id: 'turn-1', created_at: '', updated_at: '',
      },
      turns: [{ id: 'turn-1', task_id: 'task-1', sequence: 1, prompt: 'Deploy it', mode: 'interactive', status: 'waiting_input', created_at: '' }],
      messages: [{ id: 'message-1', turn_id: 'turn-1', sequence: 1, role: 'user', content: 'Deploy it', status: 'complete', created_at: '' }],
      interactions: [{
        id: 'input-1', turn_id: 'turn-1', sequence: 2, type: 'user_input', status: 'pending',
        question: 'Which region?', choices: ['East US', 'West Europe'], allow_freeform: true, actions: [], created_at: '',
      }],
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
    render(<CopilotConversation sb={sandbox} agent="github-copilot" setAgent={vi.fn()} onError={vi.fn()} toast={vi.fn()} refresh={vi.fn()} />)

    expect(await screen.findByText('Which region?')).toBeTruthy()
    fireEvent.click(screen.getByTestId('copilot-choice-input-1-0'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      '/v1/sandboxes/sandbox-1/conversation/interactions/input-1/answer',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ answer: 'East US' }) }),
    ))
  })
})
