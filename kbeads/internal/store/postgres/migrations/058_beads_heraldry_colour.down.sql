-- Back to the five-token form: drop the colour word, except where two live
-- beads of one project bear the same places on different colours, which the
-- five-token register cannot hold apart; those keep their colour (a daemon
-- before 058 reads them as opaque and never matches them). Idempotent: a
-- five-token row is left alone.
UPDATE beads b
   SET heraldry = substr(b.heraldry, strpos(b.heraldry, ' ') + 1)
 WHERE b.heraldry IS NOT NULL
   AND array_length(string_to_array(b.heraldry, ' '), 1) = 6
   AND NOT EXISTS (
       SELECT 1 FROM beads o
        WHERE o.id <> b.id
          AND o.heraldry IS NOT NULL
          AND o.deleted_at IS NULL
          AND o.project IS NOT DISTINCT FROM b.project
          AND substr(o.heraldry, strpos(o.heraldry, ' ') + 1) = substr(b.heraldry, strpos(b.heraldry, ' ') + 1));
