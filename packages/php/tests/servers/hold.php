<?php

// A script for `php -S`, for HandlerTest: calls the client's holdOn() twice
// in one request, as a check that dispatches for two jobs does, and answers
// with the time limit after each, as JSON.

declare(strict_types=1);

require __DIR__ . '/../../vendor/autoload.php';

$hold = new ReflectionMethod(Cronwatch\Cronwatch::class, 'holdOn');
set_time_limit(30);
$hold->invoke(null);
$first = ini_get('max_execution_time');
// Spent some of the request: set back, so a second move would show.
set_time_limit(30);
$hold->invoke(null);
$second = ini_get('max_execution_time');
header('content-type: application/json');
echo json_encode(['first' => $first, 'second' => $second]);
