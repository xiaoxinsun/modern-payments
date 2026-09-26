CREATE FUNCTION app.validate_posting_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT count(*) FROM app.posting_entries WHERE posting_id = NEW.posting_id) < 2
       OR EXISTS (
          SELECT currency
          FROM app.posting_entries
          WHERE posting_id = NEW.posting_id
          GROUP BY currency
          HAVING sum(CASE WHEN side = 'DEBIT' THEN amount_minor ELSE 0 END)
               <> sum(CASE WHEN side = 'CREDIT' THEN amount_minor ELSE 0 END)
       )
    THEN
      RAISE EXCEPTION 'posting % is not balanced within each currency', NEW.posting_id
        USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER posting_entries_balanced
AFTER INSERT ON app.posting_entries
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION app.validate_posting_balance();

CREATE TRIGGER operations_immutable
BEFORE UPDATE OR DELETE ON app.operations
FOR EACH ROW EXECUTE FUNCTION app.reject_immutable_mutation();

CREATE FUNCTION app.protect_hold_history() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
      RAISE EXCEPTION 'holds are retained financial history' USING ERRCODE = '55000';
    END IF;
    IF OLD.hold_id <> NEW.hold_id
       OR OLD.account_id <> NEW.account_id
       OR OLD.reserve_operation_id <> NEW.reserve_operation_id
       OR OLD.amount_minor <> NEW.amount_minor
       OR OLD.currency <> NEW.currency
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
       OR OLD.created_at <> NEW.created_at
       OR OLD.status <> 'ACTIVE'
       OR NEW.status NOT IN ('CONSUMED', 'RELEASED')
    THEN
      RAISE EXCEPTION 'hold identity, amount, history, or terminal state is immutable'
        USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER holds_history_guard
BEFORE UPDATE OR DELETE ON app.holds
FOR EACH ROW EXECUTE FUNCTION app.protect_hold_history();

