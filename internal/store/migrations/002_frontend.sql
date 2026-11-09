ALTER TABLE history DROP CONSTRAINT history_diff_check;
ALTER TABLE history ADD CONSTRAINT history_diff_check CHECK (diff <> 0);

CREATE TABLE redemption (
    request_id VARCHAR(64) PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES "user"(id),
    station_id BIGINT NOT NULL REFERENCES station(id),
    station_snapshot JSONB NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX redemption_pending_user ON redemption(user_id) WHERE status = 'pending';
CREATE INDEX redemption_pending_station ON redemption(station_id) WHERE status = 'pending';
