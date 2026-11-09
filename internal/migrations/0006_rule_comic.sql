-- Comics are handled by a separate toolchain.

ALTER TABLE rule_verdicts DROP CONSTRAINT rule_verdicts_rule_check;
ALTER TABLE rule_verdicts ADD CONSTRAINT rule_verdicts_rule_check
  CHECK (rule IN ('junk', 'foreign', 'sibling', 'comic'));
