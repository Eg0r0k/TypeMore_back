import { createParamDecorator, ExecutionContext } from '@nestjs/common';
import { Users } from '@prisma/client';

const AuthUser = createParamDecorator((_, ctx: ExecutionContext) => {
  const request = ctx.switchToHttp().getRequest();
  const user = request.user as Users;
  return user;
});

export default AuthUser;
