-- separate db for integration tests
CREATE DATABASE IF NOT EXISTS mhk_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
GRANT ALL PRIVILEGES ON mhk_test.* TO 'mhk'@'%';
