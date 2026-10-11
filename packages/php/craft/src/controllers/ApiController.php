<?php

declare(strict_types=1);

namespace Cronwatch\Craft\controllers;

use Cronwatch\Craft\Plugin;
use Cronwatch\Craft\Recorder;
use Cronwatch\Web\Dashboard;
use Cronwatch\Web\Request;
use Craft;
use craft\web\Controller;
use yii\web\NotFoundHttpException;
use yii\web\Response;

/**
 * The JSON API for @cronwatch/mcp, at /cronwatch/api/... on the site, with
 * the dashboard's token (the apiToken setting, else CRONWATCH_TOKEN) as a
 * bearer token. Without a token it is not there (404), and with one shorter
 * than Recorder::TOKEN_MIN it is shut (503, the reason logged and shown on
 * the plugin's settings). Only the /api paths are served; the dashboard's
 * pages are in the Control Panel. A check through it declares the
 * settings' jobs first, and only once the token is right: a caller without
 * it gets the dashboard's 401 having made nothing run.
 */
final class ApiController extends Controller
{
    protected array|bool|int $allowAnonymous = self::ALLOW_ANONYMOUS_LIVE;
    public $enableCsrfValidation = false;

    public function actionIndex(): Response
    {
        $recorder = Plugin::getInstance()->getRecorder();
        $token = $recorder->apiToken();
        if ($token === '') {
            throw new NotFoundHttpException();
        }
        $problem = Recorder::tokenProblem($token);
        if ($problem !== null) {
            // Shut, and said where the owner looks: Craft's log and the plugin's settings.
            Craft::error($problem, 'cronwatch');
            $response = $this->response;
            $response->format = Response::FORMAT_RAW;
            $response->setStatusCode(503);
            $response->getHeaders()->set('content-type', 'text/plain; charset=utf-8');
            $response->getHeaders()->set('cache-control', 'no-store');
            $response->content = $problem . "\n";
            return $response;
        }
        $request = Request::fromGlobals();
        $at = strpos($request->path, '/cronwatch/api');
        if ($at === false) {
            throw new NotFoundHttpException();
        }
        $cw = $recorder->client();
        // POST, or GET with a bearer, as the dashboard runs the check for either.
        $method = strtoupper($request->method);
        $check = ($method === 'POST' || $method === 'GET') && DashboardController::isCheck(substr($request->path, $at + strlen('/cronwatch')));
        if ($check && self::signedIn($request, $token, $cw->cronSecret, $method === 'POST')) {
            $recorder->prepare();
        }
        $dashboard = new Dashboard($cw, token: $token, basePath: substr($request->path, 0, $at) . '/cronwatch');
        $answer = $dashboard->handle($request);

        $response = $this->response;
        $response->format = Response::FORMAT_RAW;
        $response->setStatusCode($answer->status);
        foreach ($answer->headers as $name => $value) {
            $response->getHeaders()->set($name, $value);
        }
        $response->content = $answer->body;
        return $response;
    }

    /**
     * Whether a request to /api/check carries what the dashboard will let
     * in: the token or the client's cron secret as a bearer, or the
     * dashboard's cookie, compared in constant time. The dashboard checks
     * again.
     */
    private static function signedIn(Request $request, string $token, ?string $cronSecret, bool $orCookie = true): bool
    {
        if (preg_match('/^Bearer\s+(.+)$/is', (string) $request->header('authorization'), $m) === 1) {
            return Dashboard::secretMatches($m[1], $token) || ($cronSecret !== null && Dashboard::secretMatches($m[1], $cronSecret));
        }
        if (!$orCookie) {
            return false;
        }
        foreach (explode(';', (string) $request->header('cookie')) as $pair) {
            [$name, $value] = array_pad(explode('=', trim($pair), 2), 2, '');
            if ($name === Dashboard::COOKIE && Dashboard::secretMatches($value, Dashboard::cookieValue($token))) {
                return true;
            }
        }
        return false;
    }
}
