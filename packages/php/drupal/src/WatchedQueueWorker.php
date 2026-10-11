<?php

declare(strict_types=1);

namespace Drupal\cronwatch;

use Drupal\Core\Plugin\ContainerFactoryPluginInterface;
use Drupal\Core\Queue\QueueWorkerBase;
use Drupal\Core\Queue\QueueWorkerInterface;
use Symfony\Component\DependencyInjection\ContainerInterface;

/**
 * A watched queue worker: the real one, with each item processed a run.
 *
 * cronwatch_queue_info_alter() puts this class in the definition of every
 * watched worker and keeps the worker's own class as "cronwatch_class"; this
 * makes that worker as the plugin system would have (through its create()
 * when it has one) and hands each item to it, recording the processing as a
 * run of the worker's job (Recorder::queueItem()). What the worker throws
 * (a failure, or a RequeueException, DelayedRequeueException, or
 * SuspendQueueException asking for the item back) is recorded as a failed
 * attempt and thrown on, so the queue runner releases, delays, or keeps the
 * item as it would: failing attempts open one alert, and the attempt that
 * succeeds closes it.
 *
 * The worker the plugin manager hands out is this one, not the worker's own
 * class: Drupal has no one place that runs queue items (core's cron, Drush's
 * queue:run, and Ultimate Cron each make the worker and call processItem()
 * themselves), so the worker is the only place every item passes through.
 * A method of the worker's own is passed on to it (__call), and inner()
 * gives the worker itself, but an instanceof test of the worker's class, or
 * of an interface only it implements, is false; the settings say so beside
 * "Watched queues".
 */
final class WatchedQueueWorker extends QueueWorkerBase implements ContainerFactoryPluginInterface {

  public function __construct(
    array $configuration,
    $plugin_id,
    $plugin_definition,
    private readonly QueueWorkerInterface $inner,
    private readonly Recorder $recorder,
  ) {
    parent::__construct($configuration, $plugin_id, $plugin_definition);
  }

  /**
   * {@inheritdoc}
   */
  public static function create(ContainerInterface $container, array $configuration, $plugin_id, $plugin_definition) {
    $class = (string) $plugin_definition['cronwatch_class'];
    $definition = ['class' => $class] + $plugin_definition;
    unset($definition['cronwatch_class']);
    $inner = is_subclass_of($class, ContainerFactoryPluginInterface::class)
      ? $class::create($container, $configuration, $plugin_id, $definition)
      : new $class($configuration, $plugin_id, $definition);
    return new self($configuration, $plugin_id, $plugin_definition, $inner, $container->get('cronwatch.recorder'));
  }

  /**
   * The worker this one wraps.
   */
  public function inner(): QueueWorkerInterface {
    return $this->inner;
  }

  /**
   * A method of the worker's own, passed on to it.
   *
   * @param array<int, mixed> $arguments
   *   The method's arguments.
   */
  public function __call(string $method, array $arguments): mixed {
    if (!is_callable([$this->inner, $method])) {
      throw new \BadMethodCallException(sprintf('Call to undefined method %s::%s()', get_class($this->inner), $method));
    }
    return $this->inner->{$method}(...$arguments);
  }

  /**
   * {@inheritdoc}
   */
  public function processItem($data) {
    return $this->recorder->queueItem((string) $this->getPluginId(), (string) $this->pluginDefinition['cronwatch_class'], fn () => $this->inner->processItem($data));
  }

}
