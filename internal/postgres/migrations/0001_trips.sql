-- Rows to export. One row per completed trip, keyed by the engagement id
-- the booking system assigns, so a replayed event updates instead of
-- duplicating. Amounts are integers in the currency's minor unit.
CREATE TABLE trips (
    engagement_id     BIGINT      PRIMARY KEY,
    partner_user_id   TEXT        NOT NULL,
    booking_time      TIMESTAMPTZ NOT NULL,
    trip_end_time     TIMESTAMPTZ NOT NULL,
    service_type      TEXT        NOT NULL,
    pickup_location   TEXT        NOT NULL DEFAULT '',
    dropoff_location  TEXT        NOT NULL DEFAULT '',
    total_fare        BIGINT      NOT NULL,
    discount          BIGINT      NOT NULL DEFAULT 0,
    toll_fee          BIGINT      NOT NULL DEFAULT 0,
    insurance_fee     BIGINT      NOT NULL DEFAULT 0,
    customer_pay      BIGINT      NOT NULL,
    currency          TEXT        NOT NULL DEFAULT 'VND',
    payment_method    TEXT        NOT NULL DEFAULT '',
    status            TEXT        NOT NULL DEFAULT 'SUCCESS',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The daily export selects a window on trip_end_time.
CREATE INDEX trips_trip_end_time_idx ON trips (trip_end_time, engagement_id);
