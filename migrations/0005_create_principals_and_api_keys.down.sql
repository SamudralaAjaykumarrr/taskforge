-- Drop order matters: api_keys.principal_id references principals(id).
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS principals;
