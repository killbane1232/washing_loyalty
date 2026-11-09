CREATE TABLE "user" (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    login VARCHAR(64) NOT NULL UNIQUE,
    hash TEXT NOT NULL,
    email VARCHAR(254) NOT NULL UNIQUE,
    balance BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE station (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name VARCHAR(200) NOT NULL,
    point VARCHAR(200) NOT NULL,
    qr VARCHAR(512) NOT NULL UNIQUE,
    address VARCHAR(512) NOT NULL,
    connection_type VARCHAR(64) NOT NULL
);

CREATE TABLE history (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES "user"(id),
    station_id BIGINT NOT NULL REFERENCES station(id),
    event_id VARCHAR(128) NOT NULL UNIQUE,
    balance_after BIGINT NOT NULL CHECK (balance_after >= 0),
    diff BIGINT NOT NULL CHECK (diff > 0),
    datetime TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX history_user_datetime_idx ON history (user_id, datetime DESC, id DESC);
