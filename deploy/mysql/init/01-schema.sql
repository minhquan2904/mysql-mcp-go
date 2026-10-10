-- Seed schema for the read-only MCP server tests. Runs once on an empty data volume.
SET NAMES utf8mb4;
CREATE DATABASE testdb CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
USE testdb;

CREATE TABLE customers (
  id         INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  email      VARCHAR(255) NOT NULL,
  name       VARCHAR(100) NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uq_customers_email (email)
) ENGINE=InnoDB;

CREATE TABLE orders (
  id          INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  customer_id INT NOT NULL,
  status      ENUM('new','paid','shipped') NOT NULL DEFAULT 'new',
  total       DECIMAL(12,2) NOT NULL DEFAULT 0,
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  KEY idx_status_created (status, created_at),
  CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id)
) ENGINE=InnoDB;

CREATE TABLE order_items (
  order_id INT NOT NULL,
  line_no  INT NOT NULL,
  sku      VARCHAR(32) NOT NULL,
  qty      INT NOT NULL,
  price    DECIMAL(10,2) NOT NULL,
  PRIMARY KEY (order_id, line_no),
  CONSTRAINT fk_items_order FOREIGN KEY (order_id) REFERENCES orders (id)
) ENGINE=InnoDB;

-- Deliberately no PRIMARY KEY / UNIQUE (tests list_tables_missing_unique_indexes).
CREATE TABLE audit_log (
  event   VARCHAR(64) NULL,
  payload JSON NULL,
  at      DATETIME NULL
) ENGINE=InnoDB;

CREATE TABLE products_blob (
  id   INT NOT NULL PRIMARY KEY,
  img  BLOB NULL,
  note TEXT NULL
) ENGINE=InnoDB;

-- Scratch table for write-attempt tests; its content must never change.
CREATE TABLE it_scratch (
  id INT NOT NULL PRIMARY KEY,
  v  INT NOT NULL
) ENGINE=InnoDB;

-- ---- data (recursive CTE sequences) ----
INSERT INTO customers (email, name, created_at)
WITH RECURSIVE seq (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 20)
SELECT CONCAT('user', n, '@example.com'), CONCAT('Customer ', n), TIMESTAMP('2026-01-01 00:00:00') + INTERVAL n DAY FROM seq;

INSERT INTO orders (customer_id, status, total, created_at)
WITH RECURSIVE seq (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 50)
SELECT 1 + (n MOD 20), ELT(1 + (n MOD 3), 'new', 'paid', 'shipped'), 0, TIMESTAMP('2026-02-01 00:00:00') + INTERVAL n HOUR FROM seq;

INSERT INTO order_items (order_id, line_no, sku, qty, price)
WITH RECURSIVE seq (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 4)
SELECT o.id, seq.n, CONCAT('SKU-', LPAD(o.id * 7 + seq.n, 4, '0')), 1 + (o.id MOD 5), 9.99 + seq.n
FROM orders o JOIN seq ON seq.n <= 1 + (o.id MOD 4);

UPDATE orders o
JOIN (SELECT order_id, SUM(qty * price) AS t FROM order_items GROUP BY order_id) s ON s.order_id = o.id
SET o.total = s.t;

INSERT INTO audit_log (event, payload, at)
WITH RECURSIVE seq (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 30)
SELECT CONCAT('seed_event_', n), JSON_OBJECT('n', n, 'tags', JSON_ARRAY('a', 'b'), 'nested', JSON_OBJECT('ok', TRUE)),
       TIMESTAMP('2026-03-01 00:00:00') + INTERVAL n MINUTE FROM seq;

INSERT INTO products_blob (id, img, note) VALUES
  (1, 0xFF00FE, 'binary, not valid UTF-8'),
  (2, CAST('plain text blob' AS BINARY), 'ascii blob'),
  (3, 0x89504E470D0A1A0A, NULL);

INSERT INTO it_scratch (id, v) VALUES (1, 1);

-- ---- view / trigger ----
CREATE VIEW v_order_totals AS
SELECT o.id AS order_id, o.customer_id, COUNT(oi.line_no) AS line_count, COALESCE(SUM(oi.qty * oi.price), 0) AS items_total
FROM orders o LEFT JOIN order_items oi ON oi.order_id = o.id
GROUP BY o.id, o.customer_id;

-- Binary logging is on by default in 8.4; a MODIFIES SQL DATA function needs this (init-time only,
-- the temporary init server is discarded before the real server starts).
SET GLOBAL log_bin_trust_function_creators = 1;

DELIMITER $$
CREATE TRIGGER trg_orders_bi BEFORE INSERT ON orders
FOR EACH ROW
BEGIN
  IF NEW.total < 0 THEN
    SET NEW.total = 0;
  END IF;
END$$

-- DEFINER function with a side effect, used by v_definer to test that a SELECT through
-- a DEFINER view cannot write (MODIFIES SQL DATA, SQL SECURITY DEFINER).
CREATE FUNCTION f_log() RETURNS INT
MODIFIES SQL DATA
SQL SECURITY DEFINER
BEGIN
  INSERT INTO audit_log (event, at) VALUES ('definer', NOW());
  RETURN 1;
END$$
DELIMITER ;

CREATE SQL SECURITY DEFINER VIEW v_definer AS SELECT f_log() AS x;
