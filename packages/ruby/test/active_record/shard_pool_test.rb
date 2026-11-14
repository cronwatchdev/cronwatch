# frozen_string_literal: true

require_relative "../support/active_record_helper"

# The pool the store writes through on Postgres is its own, on the default
# shard. An app that swaps shards (ActiveRecord::Base.connected_to(shard:),
# as Rails' ShardSelector middleware does around every request) must still
# reach it. The pool's class is made from a database config whatever its
# adapter, so SQLite stands in for Postgres here.
class ActiveRecordStoreShardPoolTest < Minitest::Test
  Store = Cronwatch::Stores::ActiveRecord

  def config(name)
    ::ActiveRecord::DatabaseConfigurations::HashConfig.new(
      "test", name, { adapter: "sqlite3", database: File.join(ARSupport::TMP, "#{name}-#{SecureRandom.hex(4)}.sqlite3") },
    )
  end

  def one(pool)
    pool.with_connection { |conn| conn.select_value("SELECT 1") }
  end

  def test_the_stores_own_pool_is_found_while_the_app_is_on_another_shard
    db_config = config("swapped")
    pool = Store.own_pool(db_config)
    assert_equal 1, one(pool)
    ::ActiveRecord::Base.connected_to(shard: :shard_two) do
      assert_same pool, Store.own_pool(db_config), "the same pool, not one looked for on shard_two"
      ::ActiveRecord::Base.connected_to(role: ::ActiveRecord.writing_role, prevent_writes: false) do
        assert_equal 1, one(Store.own_pool(db_config)), "as the store asks for it, inside its own connected_to"
      end
    end
  end

  def test_a_pool_first_used_on_another_shard_is_still_on_the_default_one
    db_config = config("first")
    pool = ::ActiveRecord::Base.connected_to(shard: :shard_two) { Store.own_pool(db_config) }
    assert_equal 1, one(pool)
    assert_same pool, Store.own_pool(db_config), "found again from the default shard"
    assert_equal "first_cronwatch", pool.db_config.name
  end
end
