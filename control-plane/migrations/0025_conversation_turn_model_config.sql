-- A queued hosted-Copilot turn must preserve the model configuration selected
-- at submission time. Do not resolve these values at execution time: operator
-- defaults may change while a turn waits behind another conversation request.
ALTER TABLE conversation_turn ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_turn ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_turn ADD COLUMN context_tier TEXT NOT NULL DEFAULT 'default';
