-- Runs once when the compose MySQL volume is created: a separate database for the integration tests so they
-- do not fill the development database with test accounts.
CREATE DATABASE IF NOT EXISTS mhk_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
GRANT ALL PRIVILEGES ON mhk_test.* TO 'mhk'@'%';
