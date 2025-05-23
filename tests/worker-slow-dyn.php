<?php

/**
 * @var Goridge\RelayInterface $relay
 */

use Spiral\Goridge;
use Spiral\RoadRunner;

ini_set('display_errors', 'stderr');
require __DIR__ . "/vendor/autoload.php";

$worker = new RoadRunner\Worker(new Goridge\StreamRelay(STDIN, STDOUT));
$psr7 = new RoadRunner\Http\PSR7Worker(
	$worker,
	new \Nyholm\Psr7\Factory\Psr17Factory(),
	new \Nyholm\Psr7\Factory\Psr17Factory(),
	new \Nyholm\Psr7\Factory\Psr17Factory()
);

while ($req = $psr7->waitRequest()) {
	try {
		$resp = new \Nyholm\Psr7\Response();
		sleep(10);
		$resp->getBody()->write("hello world");

		$psr7->respond($resp);
	} catch (\Throwable $e) {
		$psr7->getWorker()->error((string)$e);
	}
}
