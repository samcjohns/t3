-- News articles published by the news market maker, newest by seq.
CREATE TABLE gateway.news (
    seq        bigserial PRIMARY KEY,
    id         text NOT NULL UNIQUE,
    tick       bigint NOT NULL,
    created_at timestamptz NOT NULL,
    symbol     text NOT NULL,
    headline   text NOT NULL,
    body       text NOT NULL,
    price      bigint NOT NULL,
    target     bigint NOT NULL
);

CREATE INDEX news_symbol ON gateway.news (symbol, seq);
