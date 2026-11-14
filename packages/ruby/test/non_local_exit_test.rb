# frozen_string_literal: true

require_relative "test_helper"

# Ruby only: a block can be left without returning or raising, by a `return`
# from the method around it, a `break`, or a `throw`. JavaScript has no such
# exit, so the SDK has no case for it. The run is over all the same: it is
# finished as one that returned nil, and the jump goes where it was going.
class NonLocalExitTest < Minitest::Test
  include TestHelpers

  def returning(job)
    job.run do |ctx|
      ctx.log("nothing to do")
      ctx.metric(:rows, 0)
      return :returned
    end
    :not_reached
  end

  def breaking(job)
    job.run do |ctx|
      ctx.log("nothing to do")
      ctx.metric(:rows, 0)
      break :broke
    end
  end

  def throwing(job)
    catch(:halt) do
      job.run do |ctx|
        ctx.log("nothing to do")
        ctx.metric(:rows, 0)
        throw :halt, :thrown
      end
      :not_reached
    end
  end

  EXITS = { returning: :returned, breaking: :broke, throwing: :thrown }.freeze

  def test_a_block_left_by_return_break_or_throw_finishes_its_run_and_the_jump_carries_on
    EXITS.each do |exit, value|
      errors = []
      cw, clock, capture = make(on_error: ->(e, where) { errors << [where, e.message] })
      job = cw.job("sync", timeout: "30m")
      clock.advance(5 * SEC)

      assert_equal value, send(exit, job), "#{exit}: the caller gets what the jump carried"

      run = cw.runs("sync").first
      assert_equal :ok, run.status, "#{exit}: the run is finished, not left running"
      assert_equal "nothing to do", run.output, "#{exit}: its output is kept"
      assert_equal({ "rows" => 0 }, run.metrics, "#{exit}: and its metrics")
      assert_equal clock.now, run.finished_at
      assert_empty cw.store.running_runs

      clock.advance(31 * MIN)
      cw.check
      assert_empty capture.types, "#{exit}: no stuck alert once the timeout has passed"
      assert_equal :ok, cw.runs("sync").first.status
      assert_empty errors
    end
  end

  def test_a_run_left_by_a_jump_is_judged_like_any_other
    EXITS.each_key do |exit|
      cw, _, capture = make
      job = cw.job("sync", expect: "Report written")
      send(exit, job)
      run = cw.runs("sync").first
      assert_equal :failed, run.status, "#{exit}: expect is checked"
      assert_equal 'Output did not contain "Report written"', run.error
      assert_equal [:failed], capture.types, "#{exit}: and the failure alerts"
    end
  end

  def test_a_jump_closes_what_a_failure_opened
    cw, _, capture = make
    job = cw.job("sync")
    assert_raises(RuntimeError) { job.run { raise "boom" } }
    breaking(job)
    assert_equal %i[failed recovered], capture.types
  end

  def test_a_store_that_fails_while_a_jump_is_recorded_is_reported_and_the_jump_still_lands
    errors = []
    broken = []
    store = Flaky.new(Cronwatch::Stores::Memory.new, broken)
    cw, = make(store: store, on_error: ->(e, where) { errors << [where, e.message] })
    job = cw.job("sync")
    result = job.run do
      broken.push(:get_state, :update_run_if, :update_run, :get_run, :insert_run)
      break :broke
    end
    assert_equal :broke, result
    refute_empty errors
    assert(errors.all? { |where, _| where.end_with?("sync") }, errors.inspect)
  end

  def test_a_run_by_name_is_finished_too
    cw, = make
    result = cw.run("by-name") { break :broke }
    assert_equal :broke, result
    assert_equal :ok, cw.runs("by-name").first.status
  end
end
