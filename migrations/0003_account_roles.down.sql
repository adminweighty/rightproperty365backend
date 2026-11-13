-- Reverts 0003_account_roles. Destroys every account-role selection,
-- including pending requests for professional roles.
DROP TABLE IF EXISTS user_account_roles;
