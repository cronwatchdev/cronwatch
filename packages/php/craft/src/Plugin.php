<?php

declare(strict_types=1);

namespace Cronwatch\Craft;

use Cronwatch\Bridge\ChannelSettings;
use Cronwatch\Craft\models\Settings;
use Craft;
use craft\base\Model;
use craft\base\Plugin as BasePlugin;
use craft\console\ErrorHandler as ConsoleErrorHandler;
use craft\events\ExceptionEvent;
use craft\events\RegisterUrlRulesEvent;
use craft\events\RegisterUserPermissionsEvent;
use craft\services\UserPermissions;
use craft\web\UrlManager;
use yii\base\ActionEvent;
use yii\base\Controller;
use yii\base\Event;
use yii\queue\ExecEvent;
use yii\queue\Queue;

/**
 * CronWatch for Craft CMS: queue jobs and console commands watched, the
 * check (`craft cronwatch/check`), the store in Craft's database, the
 * dashboard in the Control Panel, and the channels in the plugin settings.
 *
 * @property-read Recorder $recorder
 */
final class Plugin extends BasePlugin
{
    /** Add or change alert channels (an AlertsEvent). */
    public const EVENT_ALERTS = 'cronwatchAlerts';
    /** Silence, forget, and check jobs from the dashboard (viewing it is accessPlugin-cronwatch). */
    public const PERMISSION_MANAGE = 'cronwatch-manage';

    public string $schemaVersion = '1.0.0';
    public bool $hasCpSettings = true;
    public bool $hasCpSection = true;

    private ?Recorder $recorderInstance = null;

    public function getRecorder(): Recorder
    {
        return $this->recorderInstance ??= new Recorder($this);
    }

    public function init(): void
    {
        parent::init();
        $recorder = fn (): Recorder => $this->getRecorder();

        // Queue jobs that opt in, each attempt a run (every queue: Craft's and any other yii2-queue).
        Event::on(Queue::class, Queue::EVENT_BEFORE_EXEC, fn (ExecEvent $e) => $recorder()->jobStarting($e));
        Event::on(Queue::class, Queue::EVENT_AFTER_EXEC, fn (ExecEvent $e) => $recorder()->jobFinished($e));
        Event::on(Queue::class, Queue::EVENT_AFTER_ERROR, fn (ExecEvent $e) => $recorder()->jobFailed($e));
        // An attempt another listener cancelled gets no "after" event: the queue's end of it takes the run back.
        Event::on(\craft\queue\Queue::class, \craft\queue\Queue::EVENT_AFTER_EXEC_AND_RELEASE, fn (ExecEvent $e) => $recorder()->jobAbandoned((string) $e->id));

        if (Craft::$app->getRequest()->getIsConsoleRequest()) {
            // Console commands the settings list or a WatchCommand behavior marks.
            Event::on(\yii\console\Controller::class, Controller::EVENT_BEFORE_ACTION, fn (ActionEvent $e) => $recorder()->commandStarting($e));
            Event::on(\yii\console\Controller::class, Controller::EVENT_AFTER_ACTION, fn (ActionEvent $e) => $recorder()->commandFinished($e));
            // queue/exec, the child process a worker runs one job in: any attempt still open was cancelled.
            Event::on(\yii\queue\cli\Command::class, Controller::EVENT_AFTER_ACTION, fn (ActionEvent $e) => $e->action->id === 'exec' ? $recorder()->jobAbandoned() : null);
            Event::on(ConsoleErrorHandler::class, ConsoleErrorHandler::EVENT_BEFORE_HANDLE_EXCEPTION, fn (ExceptionEvent $e) => $recorder()->commandFailed($e->exception));
        }

        Event::on(UrlManager::class, UrlManager::EVENT_REGISTER_CP_URL_RULES, function (RegisterUrlRulesEvent $event): void {
            $event->rules['cronwatch'] = 'cronwatch/dashboard/index';
            $event->rules['cronwatch/view'] = 'cronwatch/dashboard/view';
        });
        Event::on(UrlManager::class, UrlManager::EVENT_REGISTER_SITE_URL_RULES, function (RegisterUrlRulesEvent $event): void {
            // The JSON API for @cronwatch/mcp: 404 until a token is set. GET
            // /cronwatch/api itself (the library and its version) has no path after it.
            $event->rules['cronwatch/api'] = 'cronwatch/api/index';
            $event->rules['cronwatch/api/<path:.*>'] = 'cronwatch/api/index';
        });
        Event::on(UserPermissions::class, UserPermissions::EVENT_REGISTER_PERMISSIONS, function (RegisterUserPermissionsEvent $event): void {
            $event->permissions[] = [
                'heading' => 'CronWatch',
                'permissions' => [
                    self::PERMISSION_MANAGE => ['label' => 'Silence, forget, and check jobs from the dashboard'],
                ],
            ];
        });
    }

    protected function createSettingsModel(): ?Model
    {
        return new Settings();
    }

    protected function settingsHtml(): ?string
    {
        $overrides = $this->overrides();
        $settings = $this->getSettings();
        $values = [];
        $saved = [];
        foreach ($settings->attributes() as $name) {
            $value = $settings->{$name};
            if (!is_string($value)) {
                continue;
            }
            if (in_array($name, $overrides, true)) {
                // Set in config/cronwatch.php: the field is shown disabled, and empty, so
                // a value the file reads from the environment never reaches the page.
                $value = '';
            } elseif (in_array($name, Settings::secretAttributes(), true) && $value !== '' && !self::isEnvReference($value)) {
                // A secret written in plain is never shown again; its field left blank keeps it.
                $saved[] = $name;
                $value = '';
            }
            $values[$name] = $value;
        }
        return Craft::$app->getView()->renderTemplate('cronwatch/_settings.twig', [
            'settings' => $settings,
            'values' => $values,
            'saved' => $saved,
            // Settings config/cronwatch.php sets win over the form, so their fields are shown disabled.
            'overrides' => $overrides,
            'secrets' => Settings::secretAttributes(),
            'sections' => self::channelSections($settings),
            // A token too short for the JSON API, which stays shut until it is longer.
            'apiTokenProblem' => Recorder::tokenProblem($this->getRecorder()->apiToken()),
        ]);
    }

    /**
     * The settings config/cronwatch.php sets, which win over the form.
     *
     * @return list<string>
     */
    private function overrides(): array
    {
        $file = Craft::$app->getConfig()->getConfigFromFile($this->handle);
        return is_array($file) ? array_values(array_map('strval', array_keys($file))) : [];
    }

    /** "$NAME" or "${NAME}" alone: an environment variable's name, not a secret. */
    private static function isEnvReference(string $value): bool
    {
        return preg_match('/^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})$/D', trim($value)) === 1;
    }

    /**
     * The settings config/cronwatch.php sets (the API token, the commands,
     * and the queue jobs among them) are saved as project config held them,
     * never as the file has them: a save from the Control Panel would
     * otherwise copy the file's values, an environment variable's resolved
     * among them, into config/project.
     */
    public function beforeSaveSettings(): bool
    {
        $settings = $this->getSettings();
        if ($settings instanceof Settings) {
            $stored = Craft::$app->getProjectConfig()->get("plugins.{$this->handle}.settings");
            $stored = is_array($stored) ? \craft\helpers\ProjectConfig::unpackAssociativeArrays($stored) : [];
            $settings->saving($this->overrides(), $stored);
        }
        return parent::beforeSaveSettings();
    }

    /**
     * The other channels for the settings form, by section: each provider
     * with its fields named as the model names them, open when one is set.
     *
     * @return list<array{title: string, intro: ?string, providers: list<array<string, mixed>>}>
     */
    private static function channelSections(?Model $settings): array
    {
        $sections = [
            ChannelSettings::CHAT => ['title' => 'Chat', 'intro' => null, 'providers' => []],
            ChannelSettings::EMAIL => ['title' => 'Email through a provider', 'intro' => 'Email alerts to, above, already sends through Craft’s mailer. Use a provider when the site cannot send mail reliably.', 'providers' => []],
            ChannelSettings::SMS => ['title' => 'Text messages', 'intro' => null, 'providers' => []],
            ChannelSettings::TRACKERS => ['title' => 'Error trackers', 'intro' => null, 'providers' => []],
        ];
        foreach (ChannelSettings::providers() as $provider => $spec) {
            $fields = [];
            $open = false;
            foreach ($spec['fields'] as $field => $f) {
                $name = ChannelSettings::camel("{$provider}_{$field}");
                $open = $open || ($settings !== null && (string) $settings->{$name} !== '');
                $fields[] = ['name' => $name] + $f;
            }
            $sections[$spec['section']]['providers'][] = ['label' => $spec['label'], 'open' => $open, 'fields' => $fields];
        }
        return array_values($sections);
    }

    public function afterSaveSettings(): void
    {
        $settings = $this->getSettings();
        if ($settings instanceof Settings) {
            $settings->saved();
        }
        parent::afterSaveSettings();
        $this->recorderInstance?->reset();
    }
}
