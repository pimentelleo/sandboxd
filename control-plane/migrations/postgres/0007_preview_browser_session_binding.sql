-- Preview authority must remain subordinate to the console session that issued
-- it. Existing rows intentionally receive an empty binding and fail closed.
ALTER TABLE preview_ticket ADD COLUMN browser_session_token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE preview_session ADD COLUMN browser_session_token_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX preview_ticket_browser_session_idx ON preview_ticket(browser_session_token_hash);
CREATE INDEX preview_session_browser_session_idx ON preview_session(browser_session_token_hash);
