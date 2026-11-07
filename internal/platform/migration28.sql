ALTER TABLE line_offerings
 ADD COLUMN notes text NOT NULL DEFAULT '' CHECK (char_length(notes) <= 500);

ALTER TABLE line_offerings
 DROP CONSTRAINT line_offerings_source_market_check,
 DROP CONSTRAINT line_offerings_destination_market_check;

ALTER TABLE line_offerings
 ADD CONSTRAINT line_offerings_source_market_check
  CHECK (char_length(source_market) BETWEEN 0 AND 80),
 ADD CONSTRAINT line_offerings_destination_market_check
  CHECK (char_length(destination_market) BETWEEN 0 AND 80);
