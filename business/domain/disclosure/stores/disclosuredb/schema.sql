-- Every head of the disclosure chain this machine has ever been shown.
--
-- The point of the table is that it is HERE. Each row is a claim the server
-- made on a day, written down on the owner's own disk where the server cannot
-- reach it; showing a log today that disagrees with one of these rows is the
-- only kind of tampering a client can detect without trusting the thing it is
-- checking. See business/domain/disclosure/disclosurebus.
--
-- It holds no secret. A head is a hash of a hash of rows that themselves
-- record no password, which is why it is safe to keep forever and pointless to
-- steal.
CREATE TABLE IF NOT EXISTS disclosure_witness (
    -- The chain length this head was the head of. The primary key, so seeing
    -- the same head again on every page view does not write a row and does not
    -- move seen_at: the date this machine can vouch from is the FIRST sighting.
    count   INTEGER PRIMARY KEY,

    -- SHA-256 of the newest entry, lowercase hex. Empty only for count 0,
    -- which is a provisioned machine that has never been given its password.
    hash    TEXT NOT NULL,

    -- When the server said that entry was written, RFC3339 with the offset.
    at      TEXT NOT NULL DEFAULT '',

    -- When this machine first saw it, which is the sentence the page prints.
    seen_at TEXT NOT NULL
);
