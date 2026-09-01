ALTER TABLE principal ADD COLUMN roles TEXT NOT NULL DEFAULT '[]';
ALTER TABLE login_transaction ADD COLUMN return_location TEXT NOT NULL DEFAULT '/';
