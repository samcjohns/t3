-- Progress of Journal.Compact: every tick at or before through_tick has been
-- archived and slimmed, and the orders it closed unfilled are deleted.
CREATE TABLE engine.compaction (
    singleton    boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    through_tick bigint NOT NULL
);

INSERT INTO engine.compaction (through_tick) VALUES (0);
