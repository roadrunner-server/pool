<?php
 /**
  * @var Goridge\RelayInterface $relay
  */

use Spiral\Goridge;
use Spiral\RoadRunner;

$rr = new RoadRunner\Worker($relay);

$counter = 0;

while ($in = $rr->waitPayload()) {
    try {
        $counter++;

        if ($counter % 2 === 0) {
            $rr->error('test error');
            continue;
        }

        sleep(100);
        $rr->respond(new RoadRunner\Payload((string)$in->body));
    } catch (\Throwable $e) {
        $rr->error((string)$e);
    }
}
