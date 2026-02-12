-- Menu items for foods, drinks, desserts (used by main bot and adder bot)
CREATE TABLE IF NOT EXISTS menu_items (
  id BIGSERIAL PRIMARY KEY,
  category TEXT NOT NULL CHECK (category IN ('food', 'drink', 'dessert')),
  name TEXT NOT NULL,
  price BIGINT NOT NULL CHECK (price >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_menu_items_category ON menu_items(category);

-- Seed default items if table is empty
INSERT INTO menu_items (category, name, price)
SELECT v.cat, v.n, v.p FROM (VALUES
  ('food'::text, '🍕 Pizza'::text, 50000::bigint),
  ('food', '🍔 Burger', 35000),
  ('food', '🍟 Fries', 15000),
  ('food', '🥗 Salad', 25000),
  ('drink', '🥤 Cola', 8000),
  ('dessert', '🍰 Cake', 20000)
) AS v(cat, n, p)
WHERE NOT EXISTS (SELECT 1 FROM menu_items LIMIT 1);
