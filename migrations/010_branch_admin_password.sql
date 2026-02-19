-- Store unique password (bcrypt hash) per branch admin for ADDER bot login
ALTER TABLE branch_admins ADD COLUMN IF NOT EXISTS password_hash TEXT;
