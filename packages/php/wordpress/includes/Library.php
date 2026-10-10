<?php

declare(strict_types=1);

namespace Cronwatch\WordPress;

if (!defined('ABSPATH')) {
    exit;
}

/**
 * Which copy of the library the plugin runs on. The plugin bundles the
 * library (lib/src), but a copy another plugin or the site's Composer
 * autoloader loaded first wins over it, and one of another version may lack
 * what the plugin needs: the plugin then stays off and says why, rather
 * than failing on every page.
 */
final class Library
{
    /** The bundled library's version, read from its source without loading it. */
    public static function bundled(string $pluginDir): ?string
    {
        foreach ([$pluginDir . '/lib/src/Cronwatch.php', dirname($pluginDir) . '/src/Cronwatch.php'] as $file) {
            if (is_file($file) && preg_match("/const VERSION = '([^']+)'/", (string) file_get_contents($file), $m) === 1) {
                return $m[1];
            }
        }
        return null;
    }

    /**
     * The version of the library that loaded, when it is not the bundled
     * one's; null when they agree, or when either cannot be told.
     */
    public static function mismatch(string $pluginDir): ?string
    {
        $bundled = self::bundled($pluginDir);
        if ($bundled === null || !class_exists(\Cronwatch\Cronwatch::class) || !defined(\Cronwatch\Cronwatch::class . '::VERSION')) {
            return null;
        }
        $loaded = (string) constant(\Cronwatch\Cronwatch::class . '::VERSION');
        return $loaded === $bundled ? null : $loaded;
    }

    /** The admin notice shown while another version of the library is loaded. */
    public static function notice(string $loaded, string $bundled): void
    {
        if (!current_user_can('activate_plugins')) {
            return;
        }
        echo '<div class="notice notice-error"><p>' . esc_html(sprintf(
            /* translators: 1: the version of the CronWatch library another plugin or the site loaded; 2: the version this plugin bundles. */
            __('CronWatch is not running: another plugin or the site\'s Composer packages load version %1$s of the CronWatch library, and this plugin needs %2$s. Update or remove the other copy, or update this plugin to match it.', 'cronwatch'),
            $loaded,
            $bundled,
        )) . '</p></div>';
    }
}
