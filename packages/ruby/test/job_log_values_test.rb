# frozen_string_literal: true

require_relative "test_helper"
require "set"

# Ruby only: every Enumerable has a to_h, and a Range's, a Set's, or an
# Enumerator's raises unless its elements are pairs. Without ActiveSupport
# (which gives each an as_json) such a value inside a logged Hash or Array
# used to raise out of job.log, failing the job over a log line.
class JobLogValuesTest < Minitest::Test
  include TestHelpers

  PLAIN = !Object.method_defined?(:as_json)

  def test_logging_a_hash_or_array_that_holds_a_range_a_set_or_an_enumerator_does_not_raise
    cw, _, capture = make
    job = cw.job("sync")
    values = [{ window: 1..5 }, { ids: Set[1, 2] }, [[1, 2].each_slice(1)], { each: [1, 2].each }, [1..2, Set["a"]]]
    result = job.run do |ctx|
      values.each { |value| ctx.log("processed", value) }
      :done
    end
    assert_equal :done, result
    run = cw.runs("sync").first
    assert_equal :ok, run.status
    assert_equal values.length, run.output.lines.length
    assert_empty capture.types
    return unless PLAIN

    assert_equal 'processed {"window":"1..5"}', run.output.lines.first.chomp
  end

  def test_a_value_whose_to_h_cannot_make_a_hash_is_written_as_its_text
    skip "ActiveSupport's as_json is loaded" unless PLAIN

    assert_equal '{"window":"1..5"}', Cronwatch::JS.json({ window: 1..5 })
    assert_equal '["1...3"]', Cronwatch::JS.json([1...3])
    assert_equal %({"ids":#{Cronwatch::JS.json(Set[1, 2].to_s)}}), Cronwatch::JS.json({ ids: Set[1, 2] })
    not_a_hash = Class.new do
      def to_h = :nope
      def to_s = "odd"
    end
    assert_equal '["odd"]', Cronwatch::JS.json([not_a_hash.new])
  end

  def test_what_has_a_to_h_that_works_is_still_written_as_an_object
    pairs = Struct.new(:a, :b)
    assert_equal '{"a":1,"b":[2]}', Cronwatch::JS.json(pairs.new(1, [2]))
    assert_equal '[{"a":1}]', Cronwatch::JS.json([[[:a, 1]].each]) if PLAIN
  end
end
