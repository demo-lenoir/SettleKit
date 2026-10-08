CREATE TABLE payment_intents (
    id uuid PRIMARY KEY,
    merchant_principal text NOT NULL CHECK (length(merchant_principal) BETWEEN 1 AND 128),
    escrow_id bytea NOT NULL UNIQUE CHECK (octet_length(escrow_id) = 32),
    chain_id bigint NOT NULL CHECK (chain_id > 0),
    escrow_contract bytea NOT NULL CHECK (octet_length(escrow_contract) = 20),
    payer bytea NOT NULL CHECK (octet_length(payer) = 20),
    payee bytea NOT NULL CHECK (octet_length(payee) = 20),
    token bytea NOT NULL CHECK (octet_length(token) = 20),
    amount numeric(39,0) NOT NULL
        CHECK (amount BETWEEN 1 AND 340282366920938463463374607431768211455),
    status text NOT NULL CHECK (status IN (
        'CREATED', 'AWAITING_CHAIN', 'OBSERVED', 'CONFIRMING',
        'CONFIRMED', 'REORGED', 'RELEASED', 'REFUNDED', 'EXPIRED', 'FAILED'
    )),
    status_version bigint NOT NULL DEFAULT 0 CHECK (status_version >= 0),
    expires_at timestamptz NOT NULL,
    observed_block_hash bytea CHECK (observed_block_hash IS NULL OR octet_length(observed_block_hash) = 32),
    observed_tx_hash bytea CHECK (observed_tx_hash IS NULL OR octet_length(observed_tx_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (date_trunc('second', expires_at) = expires_at)
);

CREATE INDEX payment_intents_merchant_created_idx
    ON payment_intents (merchant_principal, created_at DESC, id DESC);

CREATE TABLE chain_blocks (
    chain_id bigint NOT NULL CHECK (chain_id > 0),
    block_hash bytea NOT NULL CHECK (octet_length(block_hash) = 32),
    block_number bigint NOT NULL CHECK (block_number >= 0),
    parent_hash bytea NOT NULL CHECK (octet_length(parent_hash) = 32),
    canonical boolean NOT NULL DEFAULT false,
    imported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, block_hash)
);

CREATE UNIQUE INDEX chain_blocks_one_canonical_per_height
    ON chain_blocks (chain_id, block_number) WHERE canonical;

CREATE TABLE chain_logs (
    chain_id bigint NOT NULL CHECK (chain_id > 0),
    block_hash bytea NOT NULL CHECK (octet_length(block_hash) = 32),
    tx_hash bytea NOT NULL CHECK (octet_length(tx_hash) = 32),
    log_index integer NOT NULL CHECK (log_index >= 0),
    escrow_id bytea NOT NULL CHECK (octet_length(escrow_id) = 32),
    event_kind text NOT NULL CHECK (event_kind IN (
        'EscrowCreated', 'EscrowFunded', 'EscrowReleased', 'EscrowRefunded', 'EscrowExpired'
    )),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    removed boolean NOT NULL DEFAULT false,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, block_hash, tx_hash, log_index),
    FOREIGN KEY (chain_id, block_hash) REFERENCES chain_blocks (chain_id, block_hash)
);

CREATE INDEX chain_logs_escrow_idx ON chain_logs (escrow_id, removed);

CREATE TABLE sync_state (
    chain_id bigint PRIMARY KEY CHECK (chain_id > 0),
    canonical_head_number bigint CHECK (canonical_head_number IS NULL OR canonical_head_number >= 0),
    canonical_head_hash bytea CHECK (
        canonical_head_hash IS NULL OR octet_length(canonical_head_hash) = 32
    ),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((canonical_head_number IS NULL) = (canonical_head_hash IS NULL)),
    FOREIGN KEY (chain_id, canonical_head_hash)
        REFERENCES chain_blocks (chain_id, block_hash)
);

CREATE TABLE payment_state_history (
    intent_id uuid NOT NULL REFERENCES payment_intents (id),
    version bigint NOT NULL CHECK (version >= 0),
    from_status text,
    to_status text NOT NULL,
    reason text NOT NULL,
    chain_block_hash bytea CHECK (chain_block_hash IS NULL OR octet_length(chain_block_hash) = 32),
    changed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (intent_id, version)
);

CREATE TABLE idempotency_records (
    principal text NOT NULL CHECK (length(principal) BETWEEN 1 AND 128),
    method text NOT NULL CHECK (method IN ('POST')),
    route text NOT NULL CHECK (length(route) BETWEEN 1 AND 256),
    key text NOT NULL CHECK (key ~ '^[A-Za-z0-9._:-]{16,128}$'),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    response_status integer NOT NULL CHECK (response_status BETWEEN 200 AND 299),
    response_body bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal, method, route, key)
);

CREATE TABLE operator_actions (
    id uuid PRIMARY KEY,
    intent_id uuid NOT NULL UNIQUE REFERENCES payment_intents (id),
    principal text NOT NULL CHECK (length(principal) BETWEEN 1 AND 128),
    kind text NOT NULL CHECK (kind IN ('RELEASE', 'REFUND')),
    call_data bytea NOT NULL CHECK (octet_length(call_data) >= 4),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY,
    intent_id uuid NOT NULL REFERENCES payment_intents (id),
    status_version bigint NOT NULL CHECK (status_version >= 0),
    kind text NOT NULL CHECK (kind IN ('PAYMENT_STATE_CHANGED', 'PAYMENT_REVERSED')),
    body bytea NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 10),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    delivered_at timestamptz,
    dead_lettered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (intent_id, status_version, kind),
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL)),
    CHECK (NOT (delivered_at IS NOT NULL AND dead_lettered_at IS NOT NULL))
);

CREATE INDEX outbox_due_idx ON outbox_events (next_attempt_at, id)
    WHERE delivered_at IS NULL AND dead_lettered_at IS NULL;
