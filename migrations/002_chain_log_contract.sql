ALTER TABLE chain_logs ADD COLUMN contract_address bytea;
UPDATE chain_logs SET contract_address = decode(repeat('00', 20), 'hex');
ALTER TABLE chain_logs ALTER COLUMN contract_address SET NOT NULL;
ALTER TABLE chain_logs ADD CONSTRAINT chain_logs_contract_address_len
    CHECK (octet_length(contract_address) = 20);
DROP INDEX chain_logs_escrow_idx;
CREATE INDEX chain_logs_escrow_idx
    ON chain_logs (chain_id, contract_address, escrow_id, removed);
