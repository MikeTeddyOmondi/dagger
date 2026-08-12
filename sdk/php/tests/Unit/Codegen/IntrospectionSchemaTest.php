<?php

declare(strict_types=1);

namespace Dagger\Tests\Unit\Codegen;

use Dagger\Codegen\Introspection\IntrospectionSchema;
use PHPUnit\Framework\Attributes\CoversClass;
use PHPUnit\Framework\Attributes\Group;
use PHPUnit\Framework\Attributes\Test;
use PHPUnit\Framework\TestCase;

#[Group('unit')]
#[CoversClass(IntrospectionSchema::class)]
class IntrospectionSchemaTest extends TestCase
{
    #[Test]
    public function itReadsTheSchemaVersion(): void
    {
        $schema = IntrospectionSchema::fromArray([
            '__schemaVersion' => 'v1.0.0-beta.9',
            '__schema' => ['types' => []],
        ]);

        self::assertSame('v1.0.0-beta.9', $schema->version);
    }
}
