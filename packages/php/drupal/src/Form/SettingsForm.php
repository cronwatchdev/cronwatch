<?php

declare(strict_types=1);

namespace Drupal\cronwatch\Form;

use Cronwatch\Bridge\ChannelSettings;
use Cronwatch\Bridge\TestAlert;
use Cronwatch\Duration;
use Cronwatch\Schedule;
use Drupal\Core\Config\ConfigFactoryInterface;
use Drupal\Core\Config\TypedConfigManagerInterface;
use Drupal\Core\Form\ConfigFormBase;
use Drupal\Core\Form\FormStateInterface;
use Drupal\Core\Queue\QueueWorkerManagerInterface;
use Drupal\Core\Url;
use Drupal\cronwatch\Recorder;
use Symfony\Component\DependencyInjection\ContainerInterface;

/**
 * CronWatch's settings: where alerts go, the grace, the schedule, queues.
 *
 * A Drupal form, so it carries Drupal's form token. Keys, tokens, and the
 * webhook's secret are password fields: never sent back to the browser,
 * kept when left blank. Whatever is saved here is configuration, and so
 * exported with it; the README shows how to keep secrets out of the export
 * by setting them in settings.php instead.
 */
final class SettingsForm extends ConfigFormBase {

  public function __construct(
    ConfigFactoryInterface $config_factory,
    TypedConfigManagerInterface $typed_config,
    private readonly Recorder $recorder,
    private readonly QueueWorkerManagerInterface $queues,
    private readonly bool $watchingCron,
  ) {
    parent::__construct($config_factory, $typed_config);
  }

  /**
   * {@inheritdoc}
   */
  public static function create(ContainerInterface $container) {
    return new self(
      $container->get('config.factory'),
      $container->get('config.typed'),
      $container->get('cronwatch.recorder'),
      $container->get('plugin.manager.queue_worker'),
      (bool) ($container->hasParameter('cronwatch.watching_cron') ? $container->getParameter('cronwatch.watching_cron') : FALSE),
    );
  }

  /**
   * {@inheritdoc}
   */
  public function getFormId() {
    return 'cronwatch_settings';
  }

  /**
   * {@inheritdoc}
   */
  protected function getEditableConfigNames() {
    return ['cronwatch.settings'];
  }

  /**
   * {@inheritdoc}
   */
  public function buildForm(array $form, FormStateInterface $form_state) {
    $config = $this->config('cronwatch.settings');
    [$schedule, $source] = $this->recorder->cronSchedule();

    $where = [
      '%schedule' => (string) $schedule,
      '@source' => $source === 'settings' ? $this->t('set below') : $this->t("Automated Cron's interval"),
    ];
    if (!$this->watchingCron) {
      $status = $this->t('Another module has replaced the cron service, so CronWatch cannot record cron runs. Queue workers and the check still work.');
    }
    elseif ($this->recorder->cronService() === 'ultimate_cron') {
      $status = $schedule === NULL
        ? $this->t("Ultimate Cron runs cron here. Each run of its jobs is recorded, each job expected on its own rules, and each cron run too, but cron itself has no schedule to be missed against: set one below, or turn on Automated Cron. The jobs' rules need cron to run at least as often as the most frequent of them.")
        : $this->t("Ultimate Cron runs cron here. Each run of its jobs is recorded, each job expected on its own rules, and each cron run too, expected on the schedule %schedule (@source), plus the grace. The jobs' rules need cron to run at least as often as the most frequent of them.", $where);
    }
    else {
      $status = $schedule === NULL
        ? $this->t('Each cron run and hook_cron is recorded, but cron has no schedule to be missed against: set one below, or turn on Automated Cron.')
        : $this->t('Each cron run and hook_cron is recorded. Cron is expected on the schedule %schedule (@source), plus the grace.', $where);
    }
    $form['status'] = [
      '#type' => 'item',
      '#title' => $this->t('Cron'),
      '#markup' => $status,
    ];
    $form['dashboard'] = [
      '#type' => 'item',
      '#markup' => $this->t('<a href=":url">Open the dashboard</a>.', [':url' => Url::fromRoute('cronwatch.dashboard')->toString()]),
    ];
    $token = Recorder::apiToken();
    if (Recorder::tokenProblem($token) !== NULL) {
      $form['api'] = [
        '#type' => 'item',
        '#title' => $this->t('JSON API'),
        '#markup' => '<div class="messages messages--error">' . $this->t("The JSON API is off: its token (\$settings['cronwatch_token'] in settings.php, or CRONWATCH_TOKEN) has @length characters, and it needs at least @min. Make a longer one, such as the output of <code>openssl rand -hex 24</code>.", ['@length' => strlen($token), '@min' => Recorder::TOKEN_MIN]) . '</div>',
      ];
    }

    $form['alerts'] = [
      '#type' => 'details',
      '#title' => $this->t('Alerts'),
      '#open' => TRUE,
      '#description' => $this->t("Nothing leaves the site until a channel is set; with none, alerts go to the site's log."),
    ];
    $form['alerts']['email_to'] = [
      '#type' => 'textfield',
      '#title' => $this->t('Email alerts to'),
      '#description' => $this->t("One or more addresses, separated by commas. Sent through the site's mail system."),
      '#default_value' => (string) $config->get('email_to'),
      '#maxlength' => 1024,
    ];
    $form['alerts']['slack_webhook_url'] = [
      '#type' => 'textfield',
      '#title' => $this->t('Slack incoming webhook URL'),
      '#default_value' => (string) $config->get('slack_webhook_url'),
      '#maxlength' => 2048,
    ];
    $form['alerts']['webhook_url'] = [
      '#type' => 'textfield',
      '#title' => $this->t('Webhook URL'),
      '#description' => $this->t('Each alert is POSTed here as JSON.'),
      '#default_value' => (string) $config->get('webhook_url'),
      '#maxlength' => 2048,
    ];
    $this->secretField($form['alerts'], 'webhook_secret', $this->t('Webhook signing secret'), $config, $this->t('When set, each request is signed with HMAC-SHA256 in the X-CronWatch-Signature header.'), 1024);

    $this->channelFields($form, $config);

    $form['jobs'] = [
      '#type' => 'details',
      '#title' => $this->t('Jobs'),
      '#open' => TRUE,
    ];
    $form['jobs']['schedule'] = [
      '#type' => 'textfield',
      '#title' => $this->t('Cron schedule'),
      '#description' => $this->t('How often cron runs: what the server\'s crontab does, as a cron expression (<code>*/15 * * * *</code>) or <code>every 1h</code>. Empty uses Automated Cron\'s interval when it is on.'),
      '#default_value' => (string) $config->get('schedule'),
    ];
    $form['jobs']['grace'] = [
      '#type' => 'textfield',
      '#title' => $this->t('Grace'),
      '#description' => $this->t('How late a run may be before it is reported missed, such as <code>10m</code> or <code>1h</code>.'),
      '#default_value' => (string) $config->get('grace'),
      '#required' => TRUE,
    ];
    $form['jobs']['check_on_cron'] = [
      '#type' => 'checkbox',
      '#title' => $this->t('Run the check at the end of each cron run'),
      '#description' => $this->t('The check finds missed and stuck runs. Run <code>drush cronwatch:check</code> from the server\'s crontab too: a check that runs with cron cannot notice cron not running.'),
      '#default_value' => (bool) $config->get('check_on_cron'),
    ];
    $options = [];
    foreach ($this->queues->getDefinitions() as $id => $definition) {
      $options[$id] = (string) ($definition['title'] ?? $id) . " ({$id})";
    }
    ksort($options);
    $form['jobs']['queues'] = [
      '#type' => 'checkboxes',
      '#title' => $this->t('Watched queues'),
      '#description' => $this->t('Every item a watched queue processes is a run (in cron, <code>drush queue:run</code> or anywhere). A worker class marked <code>#[Cronwatch\Watch]</code> is watched whatever is chosen here. A watched worker is wrapped: code that calls its own methods still reaches them, but code that tests <code>instanceof</code> its class gets FALSE, so leave such a queue unwatched.'),
      '#options' => $options,
      '#default_value' => array_values(array_intersect((array) $config->get('queues'), array_keys($options))),
      '#access' => $options !== [],
    ];

    $form = parent::buildForm($form, $form_state);
    $form['actions']['test'] = [
      '#type' => 'submit',
      '#value' => $this->t('Send a test alert'),
      '#submit' => ['::sendTest'],
      '#limit_validation_errors' => [],
    ];
    return $form;
  }

  /**
   * A secret's field: a password field, so the saved secret is never sent
   * back to the browser. Left blank, the saved one stays; "Remove it"
   * clears it.
   */
  private function secretField(array &$parent, string $key, $title, $config, $help, int $max): void {
    $saved = (string) $config->get($key) !== '';
    $parent[$key] = [
      '#type' => 'password',
      '#title' => $title,
      '#maxlength' => $max,
      '#attributes' => ['autocomplete' => 'new-password'],
    ];
    $description = $saved ? $this->t('One is saved. Leave blank to keep it.') : NULL;
    if ($help !== NULL) {
      $description = $description === NULL ? $help : $this->t('@saved @help', ['@saved' => $description, '@help' => $help]);
    }
    if ($description !== NULL) {
      $parent[$key]['#description'] = $description;
    }
    if ($saved) {
      $parent["{$key}_clear"] = [
        '#type' => 'checkbox',
        '#title' => $this->t('Remove it'),
      ];
    }
  }

  /**
   * What a secret's field means once submitted: the one typed in, else
   * nothing when "Remove it" is ticked, else the saved one.
   */
  private function secretValue(FormStateInterface $form_state, string $key, bool $trim): string {
    $typed = (string) $form_state->getValue($key);
    if ($form_state->getValue("{$key}_clear")) {
      return '';
    }
    if (trim($typed) !== '') {
      return $trim ? trim($typed) : $typed;
    }
    return (string) $this->config('cronwatch.settings')->get($key);
  }

  /**
   * The keys ChannelSettings marks as secrets.
   *
   * @return list<string>
   */
  private static function secretKeys(): array {
    $keys = [];
    foreach (ChannelSettings::providers() as $provider => $spec) {
      foreach ($spec['fields'] as $field => $f) {
        if ($f['kind'] === 'secret') {
          $keys[] = "{$provider}_{$field}";
        }
      }
    }
    return $keys;
  }

  /**
   * The other channels' fields, by section, each provider folded away
   * until one of its fields is set.
   */
  private function channelFields(array &$form, $config): void {
    $sections = [
      ChannelSettings::CHAT => $this->t('Chat'),
      ChannelSettings::EMAIL => $this->t('Email through a provider'),
      ChannelSettings::SMS => $this->t('Text messages'),
      ChannelSettings::TRACKERS => $this->t('Error trackers'),
    ];
    $form['channels'] = [
      '#type' => 'details',
      '#title' => $this->t('More channels'),
      '#open' => TRUE,
      '#description' => $this->t('Each channel sends once all of its required fields are set. Email alerts to, above, already sends through the site\'s own mail; use a provider when the site cannot send mail reliably.'),
    ];
    foreach ($sections as $section => $title) {
      $form['channels'][$section] = [
        '#type' => 'fieldset',
        '#title' => $title,
      ];
    }
    foreach (ChannelSettings::providers() as $provider => $spec) {
      $set = FALSE;
      foreach (array_keys($spec['fields']) as $field) {
        $set = $set || (string) $config->get("{$provider}_{$field}") !== '';
      }
      $group = [
        '#type' => 'details',
        // A provider's name, the same in every language.
        '#title' => $spec['label'],
        '#open' => $set,
      ];
      foreach ($spec['fields'] as $field => $f) {
        $key = "{$provider}_{$field}";
        if ($f['kind'] === 'secret') {
          // phpcs:ignore Drupal.Semantics.FunctionT.NotLiteralString
          $title = $f['required'] ? $this->t($f['label']) : $this->t('@label (optional)', ['@label' => $this->t($f['label'])]);
          // phpcs:ignore Drupal.Semantics.FunctionT.NotLiteralString
          $this->secretField($group, $key, $title, $config, isset($f['help']) ? $this->t($f['help']) : NULL, 2048);
          continue;
        }
        $group[$key] = [
          '#type' => $f['kind'] === 'choice' ? 'select' : 'textfield',
          // phpcs:ignore Drupal.Semantics.FunctionT.NotLiteralString -- the labels and help ChannelSettings holds, translated where a translation exists.
          '#title' => $this->t($f['label']),
          '#default_value' => (string) $config->get($key),
          '#maxlength' => 2048,
        ];
        if (isset($f['help'])) {
          // phpcs:ignore Drupal.Semantics.FunctionT.NotLiteralString
          $group[$key]['#description'] = $this->t($f['help']);
        }
        if ($f['kind'] === 'choice') {
          $group[$key]['#options'] = $f['options'] ?? [];
          unset($group[$key]['#maxlength']);
        }
        if (!$f['required'] && $f['kind'] !== 'choice') {
          $group[$key]['#title'] = $this->t('@label (optional)', ['@label' => $group[$key]['#title']]);
        }
      }
      $form['channels'][$spec['section']][$provider] = $group;
    }
  }

  /**
   * {@inheritdoc}
   */
  public function validateForm(array &$form, FormStateInterface $form_state) {
    parent::validateForm($form, $form_state);
    try {
      Duration::parse(trim((string) $form_state->getValue('grace')), 'grace');
    }
    catch (\Throwable $error) {
      $form_state->setErrorByName('grace', $error->getMessage());
    }
    $schedule = trim((string) $form_state->getValue('schedule'));
    if ($schedule !== '') {
      try {
        Schedule::parse($schedule);
      }
      catch (\Throwable $error) {
        $form_state->setErrorByName('schedule', $error->getMessage());
      }
    }
    $secrets = self::secretKeys();
    $problems = ChannelSettings::problems(fn (string $key): string => in_array($key, $secrets, TRUE)
      ? $this->secretValue($form_state, $key, TRUE)
      : (string) $form_state->getValue($key));
    foreach ($problems as $key => $message) {
      $form_state->setErrorByName($key, $message);
    }
    foreach (['slack_webhook_url', 'webhook_url'] as $key) {
      $url = trim((string) $form_state->getValue($key));
      if ($url !== '' && preg_match('#^https?://[^/\s]+#i', $url) !== 1) {
        $form_state->setErrorByName($key, $this->t('Enter an http or https URL.'));
      }
    }
  }

  /**
   * {@inheritdoc}
   */
  public function submitForm(array &$form, FormStateInterface $form_state) {
    $queues = array_values(array_filter((array) $form_state->getValue('queues'), fn ($value) => is_string($value) && $value !== ''));
    $before = (array) $this->config('cronwatch.settings')->get('queues');
    $config = $this->config('cronwatch.settings');
    $secrets = self::secretKeys();
    foreach (array_keys(ChannelSettings::defaults()) as $key) {
      $config->set($key, in_array($key, $secrets, TRUE) ? $this->secretValue($form_state, $key, TRUE) : trim((string) $form_state->getValue($key)));
    }
    $config
      ->set('email_to', trim((string) $form_state->getValue('email_to')))
      ->set('slack_webhook_url', trim((string) $form_state->getValue('slack_webhook_url')))
      ->set('webhook_url', trim((string) $form_state->getValue('webhook_url')))
      // Signed with exactly as typed, as in every port.
      ->set('webhook_secret', $this->secretValue($form_state, 'webhook_secret', FALSE))
      ->set('grace', trim((string) $form_state->getValue('grace')))
      ->set('schedule', trim((string) $form_state->getValue('schedule')))
      ->set('check_on_cron', (bool) $form_state->getValue('check_on_cron'))
      ->set('queues', $queues)
      ->save();
    if ($before !== $queues) {
      // The watched workers are chosen when the definitions are built.
      $this->queues->clearCachedDefinitions();
    }
    $this->recorder->reset();
    parent::submitForm($form, $form_state);
  }

  /**
   * Sends a test alert to every channel the saved settings name.
   */
  public function sendTest(array &$form, FormStateInterface $form_state): void {
    $channels = $this->recorder->channels();
    if ($channels === []) {
      $this->messenger()->addWarning($this->t("No alert channel is set, so alerts go to the site's log. Save a channel first."));
      return;
    }
    $site = Url::fromRoute('<front>', [], ['absolute' => TRUE])->toString(TRUE)->getGeneratedUrl();
    foreach (TestAlert::send($channels, $site) as $result) {
      if ($result['ok']) {
        $this->messenger()->addStatus($this->t('Sent to @channel.', ['@channel' => $result['channel']]) . ($result['message'] !== '' ? ' ' . $result['message'] : ''));
      }
      else {
        $this->messenger()->addError($this->t('@channel: @message', ['@channel' => $result['channel'], '@message' => $result['message']]));
      }
    }
  }

}
