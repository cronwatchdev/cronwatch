<?php

declare(strict_types=1);

// getallheaders() as Apache's SAPI has it, for the tests that read the
// Authorization header Apache keeps out of $_SERVER. The CLI has none; this
// one answers what a test puts in $GLOBALS['cronwatch_getallheaders'].
if (!function_exists('getallheaders')) {
    /** @return array<string, string> */
    function getallheaders(): array
    {
        return $GLOBALS['cronwatch_getallheaders'] ?? [];
    }
}
